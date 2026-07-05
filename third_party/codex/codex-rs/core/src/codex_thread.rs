use crate::agent::AgentStatus;
use crate::config::ConstraintResult;
use crate::session::Codex;
use crate::session::SessionSettingsUpdate;
use crate::session::SteerInputError;
use codex_features::Feature;
use codex_otel::SessionTelemetry;
use codex_protocol::ThreadId;
use codex_protocol::config_types::ApprovalsReviewer;
use codex_protocol::config_types::CollaborationMode;
use codex_protocol::config_types::Personality;
use codex_protocol::config_types::ReasoningSummary;
use codex_protocol::config_types::WindowsSandboxLevel;
use codex_protocol::error::CodexErr;
use codex_protocol::error::Result as CodexResult;
use codex_protocol::mcp::CallToolResult;
use codex_protocol::mcp::Tool;
use codex_protocol::models::ActivePermissionProfile;
use codex_protocol::models::ContentItem;
use codex_protocol::models::FunctionCallOutputBody;
use codex_protocol::models::PermissionProfile;
use codex_protocol::models::ResponseInputItem;
use codex_protocol::models::ResponseItem;
use codex_protocol::models::function_call_output_content_items_to_text;
use codex_protocol::openai_models::ReasoningEffort;
use codex_protocol::protocol::AdditionalContextEntry;
use codex_protocol::protocol::AskForApproval;
use codex_protocol::protocol::Event;
use codex_protocol::protocol::MultiAgentVersion;
use codex_protocol::protocol::Op;
use codex_protocol::protocol::RolloutItem;
use codex_protocol::protocol::SandboxPolicy;
use codex_protocol::protocol::SessionConfiguredEvent;
use codex_protocol::protocol::SessionSource;
use codex_protocol::protocol::Submission;
use codex_protocol::protocol::ThreadHistoryMode;
use codex_protocol::protocol::ThreadMemoryMode;
use codex_protocol::protocol::ThreadSource;
use codex_protocol::protocol::TokenUsageInfo;
use codex_protocol::protocol::TurnEnvironmentSelection;
use codex_protocol::protocol::TurnEnvironmentSelections;
use codex_protocol::protocol::W3cTraceContext;
use codex_protocol::user_input::UserInput;
use codex_thread_store::StoredThread;
use codex_thread_store::StoredThreadHistory;
use codex_thread_store::ThreadMetadataPatch;
use codex_thread_store::ThreadStoreError;
use codex_thread_store::ThreadStoreResult;
use codex_utils_absolute_path::AbsolutePathBuf;
use codex_utils_path_uri::LegacyAppPathString;
use codex_utils_path_uri::PathUri;
use rmcp::model::ReadResourceRequestParams;
use std::collections::BTreeMap;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use tokio::sync::Mutex;
use tokio::sync::watch;
use tokio_util::sync::CancellationToken;

use codex_rollout::state_db::StateDbHandle;

#[derive(Clone, Debug)]
pub struct ThreadConfigSnapshot {
    pub model: String,
    pub model_provider_id: String,
    pub service_tier: Option<String>,
    pub approval_policy: AskForApproval,
    pub approvals_reviewer: ApprovalsReviewer,
    pub permission_profile: PermissionProfile,
    pub active_permission_profile: Option<ActivePermissionProfile>,
    pub environments: TurnEnvironmentSelections,
    pub workspace_roots: Vec<AbsolutePathBuf>,
    pub profile_workspace_roots: Vec<AbsolutePathBuf>,
    pub ephemeral: bool,
    pub reasoning_effort: Option<ReasoningEffort>,
    pub reasoning_summary: Option<ReasoningSummary>,
    pub personality: Option<Personality>,
    pub collaboration_mode: CollaborationMode,
    pub session_source: SessionSource,
    pub history_mode: ThreadHistoryMode,
    pub forked_from_thread_id: Option<ThreadId>,
    pub parent_thread_id: Option<ThreadId>,
    pub thread_source: Option<ThreadSource>,
    pub originator: String,
}

/// Explains why `CodexThread::try_start_turn_if_idle` rejected an automatic
/// idle turn.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TryStartTurnIfIdleRejectionReason {
    /// User/client-triggered mailbox work is already queued and must take
    /// priority over extension-initiated idle work.
    PendingTriggerTurn,
    /// The thread is in Plan mode, where automatic idle work must not start a
    /// new model turn.
    PlanMode,
    /// Another turn or task is active, or the idle reservation was lost before
    /// the automatic turn could start.
    Busy,
}

/// Rejection returned when an extension asks to start automatic idle work but
/// the thread is not eligible to run it.
#[derive(Debug)]
pub struct TryStartTurnIfIdleError {
    reason: TryStartTurnIfIdleRejectionReason,
    input: Vec<ResponseItem>,
}

impl TryStartTurnIfIdleError {
    pub(crate) fn new(reason: TryStartTurnIfIdleRejectionReason, input: Vec<ResponseItem>) -> Self {
        Self { reason, input }
    }

    /// Returns the stable reason the automatic idle turn was rejected.
    pub fn reason(&self) -> TryStartTurnIfIdleRejectionReason {
        self.reason
    }

    /// Consumes the rejection and returns the original model-visible input
    /// unchanged, so callers can retry, drop, or log it explicitly.
    pub fn into_input(self) -> Vec<ResponseItem> {
        self.input
    }
}

impl ThreadConfigSnapshot {
    pub fn cwd(&self) -> &AbsolutePathBuf {
        &self.environments.legacy_fallback_cwd
    }

    pub fn environment_selections(&self) -> &[TurnEnvironmentSelection] {
        &self.environments.environments
    }

    pub fn sandbox_policy(&self) -> SandboxPolicy {
        codex_sandboxing::compatibility_sandbox_policy_for_permission_profile(
            &self.permission_profile,
            self.cwd().as_path(),
        )
    }
}

/// Thread settings overrides that app-server validates before starting a turn.
#[derive(Clone, Default)]
pub struct CodexThreadSettingsOverrides {
    pub environments: Option<TurnEnvironmentSelections>,
    pub workspace_roots: Option<Vec<AbsolutePathBuf>>,
    pub profile_workspace_roots: Option<Vec<AbsolutePathBuf>>,
    pub approval_policy: Option<AskForApproval>,
    pub approvals_reviewer: Option<ApprovalsReviewer>,
    pub sandbox_policy: Option<SandboxPolicy>,
    pub permission_profile: Option<PermissionProfile>,
    pub active_permission_profile: Option<ActivePermissionProfile>,
    pub windows_sandbox_level: Option<WindowsSandboxLevel>,
    pub model: Option<String>,
    pub effort: Option<Option<ReasoningEffort>>,
    pub summary: Option<ReasoningSummary>,
    pub service_tier: Option<Option<String>>,
    pub collaboration_mode: Option<CollaborationMode>,
    pub personality: Option<Personality>,
}

pub struct CodexThread {
    pub(crate) codex: Codex,
    pub(crate) session_source: SessionSource,
    session_configured: SessionConfiguredEvent,
    rollout_path: Option<PathBuf>,
    out_of_band_elicitation_count: Mutex<u64>,
}

#[derive(Clone, Copy)]
enum DirectToolPayloadKind {
    Function,
    Custom,
    ToolSearch,
}

struct DirectToolEntry {
    mcp_name: String,
    tool_name: codex_tools::ToolName,
    payload_kind: DirectToolPayloadKind,
    tool: Tool,
}

#[derive(Debug, Eq, PartialEq)]
pub struct BackgroundTerminalInfo {
    pub item_id: String,
    pub process_id: String,
    pub command: String,
    pub cwd: PathUri,
}

/// Conduit for the bidirectional stream of messages that compose a thread
/// (formerly called a conversation) in Codex.
impl CodexThread {
    pub(crate) fn new(
        codex: Codex,
        session_configured: SessionConfiguredEvent,
        rollout_path: Option<PathBuf>,
        session_source: SessionSource,
    ) -> Self {
        Self {
            codex,
            session_source,
            session_configured,
            rollout_path,
            out_of_band_elicitation_count: Mutex::new(0),
        }
    }

    pub async fn submit(&self, op: Op) -> CodexResult<String> {
        self.codex.submit(op).await
    }

    /// Returns the session telemetry handle for thread-scoped production instrumentation.
    pub fn session_telemetry(&self) -> SessionTelemetry {
        self.codex.session.services.session_telemetry.clone()
    }

    pub async fn shutdown_and_wait(&self) -> CodexResult<()> {
        self.codex.shutdown_and_wait().await
    }

    /// Wait until the underlying session loop has terminated.
    pub async fn wait_until_terminated(&self) {
        self.codex.session_loop_termination.clone().await;
    }

    pub(crate) async fn emit_thread_resume_lifecycle(&self) {
        for contributor in self
            .codex
            .session
            .services
            .extensions
            .thread_lifecycle_contributors()
        {
            contributor
                .on_thread_resume(codex_extension_api::ThreadResumeInput {
                    session_store: &self.codex.session.services.session_extension_data,
                    thread_store: &self.codex.session.services.thread_extension_data,
                })
                .await;
        }
    }

    pub async fn emit_thread_idle_lifecycle_if_idle(&self) {
        self.codex
            .session
            .emit_thread_idle_lifecycle_if_idle()
            .await;
    }

    #[doc(hidden)]
    pub async fn ensure_rollout_materialized(&self) {
        self.codex.session.ensure_rollout_materialized().await;
    }

    #[doc(hidden)]
    pub async fn flush_rollout(&self) -> std::io::Result<()> {
        self.codex.session.flush_rollout().await
    }

    pub async fn submit_with_trace(
        &self,
        op: Op,
        trace: Option<W3cTraceContext>,
    ) -> CodexResult<String> {
        self.codex.submit_with_trace(op, trace).await
    }

    pub async fn submit_user_input_with_client_user_message_id(
        &self,
        op: Op,
        trace: Option<W3cTraceContext>,
        client_user_message_id: Option<String>,
    ) -> CodexResult<String> {
        self.codex
            .session
            .services
            .agent_control
            .ensure_execution_capacity_for_op(self.session_configured.thread_id, &op)
            .await?;
        self.codex
            .submit_user_input_with_client_user_message_id(op, trace, client_user_message_id)
            .await
    }

    /// Persist whether this thread is eligible for future memory generation.
    pub async fn set_thread_memory_mode(&self, mode: ThreadMemoryMode) -> anyhow::Result<()> {
        self.codex.set_thread_memory_mode(mode).await
    }

    pub async fn steer_input(
        &self,
        input: Vec<UserInput>,
        additional_context: BTreeMap<String, AdditionalContextEntry>,
        expected_turn_id: Option<&str>,
        client_user_message_id: Option<String>,
        responsesapi_client_metadata: Option<HashMap<String, String>>,
    ) -> Result<String, SteerInputError> {
        self.codex
            .steer_input(
                input,
                additional_context,
                expected_turn_id,
                client_user_message_id,
                responsesapi_client_metadata,
            )
            .await
    }

    /// Injects model-visible items into the currently active turn.
    ///
    /// This is the thread-level bridge to `Session::inject_if_running` for
    /// callers that only hold a `CodexThread`.
    /// It returns the unchanged items when this thread has no active turn.
    pub async fn inject_if_running(
        &self,
        items: Vec<ResponseItem>,
    ) -> Result<(), Vec<ResponseItem>> {
        self.codex.session.inject_if_running(items).await
    }

    /// Starts an automatic regular turn with model-visible items only when idle
    /// work is allowed for this thread.
    ///
    /// This is the required entry point for extensions that want to launch
    /// model-visible work from `ThreadLifecycleContributor::on_thread_idle`.
    /// The call succeeds only if no user/client-triggered turn is queued, no
    /// task is currently active, and the thread is not in Plan mode. Active
    /// Review tasks are rejected by the active-task check because Review turns
    /// are not steerable.
    ///
    /// On rejection, the returned error includes a stable reason and carries
    /// the original `items` unchanged so the caller can decide whether to drop
    /// them, retry later, or log why no automatic turn was started.
    pub async fn try_start_turn_if_idle(
        &self,
        items: Vec<ResponseItem>,
    ) -> Result<(), TryStartTurnIfIdleError> {
        self.codex.session.try_start_turn_if_idle(items).await
    }

    pub async fn set_app_server_client_info(
        &self,
        app_server_client_name: Option<String>,
        app_server_client_version: Option<String>,
        mcp_elicitations_auto_deny: bool,
    ) -> ConstraintResult<()> {
        self.codex
            .set_app_server_client_info(
                app_server_client_name,
                app_server_client_version,
                mcp_elicitations_auto_deny,
            )
            .await
    }

    pub async fn set_openai_form_elicitation_support(&self, supported: bool) -> anyhow::Result<()> {
        self.codex
            .session
            .set_openai_form_elicitation_support(supported)
            .await
    }

    /// Preview persistent thread settings overrides without committing them.
    pub async fn preview_thread_settings_overrides(
        &self,
        overrides: CodexThreadSettingsOverrides,
    ) -> ConstraintResult<ThreadConfigSnapshot> {
        let updates = self.thread_settings_update(overrides).await;
        self.codex.session.preview_settings(&updates).await
    }

    async fn thread_settings_update(
        &self,
        overrides: CodexThreadSettingsOverrides,
    ) -> SessionSettingsUpdate {
        let CodexThreadSettingsOverrides {
            environments,
            workspace_roots,
            profile_workspace_roots,
            approval_policy,
            approvals_reviewer,
            sandbox_policy,
            permission_profile,
            active_permission_profile,
            windows_sandbox_level,
            model,
            effort,
            summary,
            service_tier,
            collaboration_mode,
            personality,
        } = overrides;
        let collaboration_mode = if let Some(collaboration_mode) = collaboration_mode {
            collaboration_mode
        } else {
            self.codex
                .session
                .collaboration_mode()
                .await
                .with_updates(model, effort, /*developer_instructions*/ None)
        };

        SessionSettingsUpdate {
            environments,
            workspace_roots,
            profile_workspace_roots,
            approval_policy,
            approvals_reviewer,
            sandbox_policy,
            permission_profile,
            active_permission_profile,
            windows_sandbox_level,
            collaboration_mode: Some(collaboration_mode),
            reasoning_summary: summary,
            service_tier,
            personality,
            ..Default::default()
        }
    }

    /// Use sparingly: this is intended to be removed soon.
    pub async fn submit_with_id(&self, sub: Submission) -> CodexResult<()> {
        self.codex.submit_with_id(sub).await
    }

    pub async fn next_event(&self) -> CodexResult<Event> {
        self.codex.next_event().await
    }

    pub async fn agent_status(&self) -> AgentStatus {
        self.codex.agent_status().await
    }

    pub async fn list_background_terminals(&self) -> Vec<BackgroundTerminalInfo> {
        self.codex.session.list_background_terminals().await
    }

    pub async fn terminate_background_terminal(&self, process_id: i32) -> bool {
        self.codex
            .session
            .terminate_background_terminal(process_id)
            .await
    }

    pub(crate) fn subscribe_status(&self) -> watch::Receiver<AgentStatus> {
        self.codex.agent_status.clone()
    }

    /// Returns the complete token usage snapshot currently cached for this thread.
    ///
    /// This accessor is intentionally narrower than direct session access: it lets
    /// app-server lifecycle paths replay restored usage after resume or fork without
    /// exposing broader session mutation authority. A caller that only reads
    /// `total_token_usage` would drop last-turn usage and make the v2
    /// `thread/tokenUsage/updated` payload incomplete.
    pub async fn token_usage_info(&self) -> Option<TokenUsageInfo> {
        self.codex.session.token_usage_info().await
    }

    /// Records a user-role session-prefix message without creating a new user turn boundary.
    pub(crate) async fn inject_user_message_without_turn(&self, message: String) {
        let item = ResponseItem::Message {
            id: None,
            role: "user".to_string(),
            content: vec![ContentItem::InputText { text: message }],
            phase: None,
            internal_chat_message_metadata_passthrough: None,
        };
        self.codex
            .session
            .inject_no_new_turn(vec![item], /*current_turn_context*/ None)
            .await;
    }

    /// Record raw Responses API items without starting a new turn.
    pub async fn inject_response_items(&self, items: Vec<ResponseItem>) -> CodexResult<()> {
        if items.is_empty() {
            return Err(CodexErr::InvalidRequest(
                "items must not be empty".to_string(),
            ));
        }

        let turn_context = self.codex.session.new_default_turn().await;
        if self.codex.session.reference_context_item().await.is_none() {
            // This history-only API runs without run_turn, so it owns its initial step.
            let step_context = self
                .codex
                .session
                .capture_step_context(Arc::clone(&turn_context))
                .await;
            self.codex
                .session
                .record_context_updates_and_set_reference_context_item(step_context.as_ref())
                .await;
        }
        self.codex
            .session
            .inject_no_new_turn(items, Some(turn_context.as_ref()))
            .await;
        self.codex.session.flush_rollout().await?;
        Ok(())
    }

    pub fn rollout_path(&self) -> Option<PathBuf> {
        self.rollout_path.clone()
    }

    pub fn session_configured(&self) -> SessionConfiguredEvent {
        self.session_configured.clone()
    }

    pub(crate) fn is_running(&self) -> bool {
        !self.codex.tx_sub.is_closed()
    }

    pub async fn guardian_trunk_rollout_path(&self) -> Option<PathBuf> {
        self.codex
            .session
            .guardian_review_session
            .trunk_rollout_path()
            .await
    }

    pub async fn load_history(
        &self,
        include_archived: bool,
    ) -> ThreadStoreResult<StoredThreadHistory> {
        let live_thread = self
            .codex
            .session
            .live_thread_for_persistence("load history")
            .map_err(|err| ThreadStoreError::Internal {
                message: err.to_string(),
            })?;
        live_thread.load_history(include_archived).await
    }

    pub async fn read_thread(
        &self,
        include_archived: bool,
        include_history: bool,
    ) -> ThreadStoreResult<StoredThread> {
        let live_thread = self
            .codex
            .session
            .live_thread_for_persistence("read thread")
            .map_err(|err| ThreadStoreError::Internal {
                message: err.to_string(),
            })?;
        live_thread
            .read_thread(include_archived, include_history)
            .await
    }

    pub async fn update_thread_metadata(
        &self,
        patch: ThreadMetadataPatch,
        include_archived: bool,
    ) -> ThreadStoreResult<StoredThread> {
        let live_thread = self
            .codex
            .session
            .live_thread_for_persistence("update thread metadata")
            .map_err(|err| ThreadStoreError::Internal {
                message: err.to_string(),
            })?;
        live_thread.update_metadata(patch, include_archived).await
    }

    /// Appends rollout items through the live thread so derived metadata stays in sync.
    pub async fn append_rollout_items(&self, items: &[RolloutItem]) -> ThreadStoreResult<()> {
        let live_thread = self
            .codex
            .session
            .live_thread_for_persistence("append rollout items")
            .map_err(|err| ThreadStoreError::Internal {
                message: err.to_string(),
            })?;
        live_thread.append_items(items).await
    }

    pub fn state_db(&self) -> Option<StateDbHandle> {
        self.codex.state_db()
    }

    pub async fn config_snapshot(&self) -> ThreadConfigSnapshot {
        self.codex.thread_config_snapshot().await
    }

    /// Returns the files that supplied the thread's loaded model instructions.
    pub async fn instruction_sources(&self) -> Vec<PathUri> {
        self.codex.instruction_sources().await
    }

    /// Returns loaded instruction sources rendered as legacy app-server path strings.
    pub async fn legacy_instruction_sources(&self) -> Vec<LegacyAppPathString> {
        self.instruction_sources()
            .await
            .into_iter()
            .map(Into::into)
            .collect()
    }

    pub async fn config(&self) -> Arc<crate::config::Config> {
        self.codex.session.get_config().await
    }

    /// Resolves the MCP runtime configuration using this thread's extension data.
    pub async fn runtime_mcp_config(&self, config: &crate::config::Config) -> codex_mcp::McpConfig {
        self.codex.session.runtime_mcp_config(config).await
    }

    /// Returns the exact MCP config, environment bindings, and manager most recently published.
    pub async fn current_mcp_runtime(&self) -> Arc<crate::session::McpRuntimeSnapshot> {
        let turn_context = self.codex.session.new_default_turn().await;
        self.codex
            .session
            .capture_step_context(turn_context)
            .await
            .mcp
            .clone()
    }

    pub fn multi_agent_version(&self) -> Option<MultiAgentVersion> {
        self.codex.session.multi_agent_version()
    }

    /// Refresh the thread's layer-backed user config state from a caller-supplied
    /// config snapshot. Thread-scoped layers and session-static settings remain
    /// unchanged.
    pub async fn refresh_runtime_config(&self, next_config: crate::config::Config) {
        self.codex.session.refresh_runtime_config(next_config).await;
    }

    pub async fn environment_selections(&self) -> Vec<TurnEnvironmentSelection> {
        self.codex.thread_environment_selections().await
    }

    pub async fn read_mcp_resource(
        &self,
        server: &str,
        uri: &str,
    ) -> anyhow::Result<serde_json::Value> {
        let result = self
            .current_mcp_runtime()
            .await
            .manager_arc()
            .read_resource(server, ReadResourceRequestParams::new(uri))
            .await?;

        Ok(serde_json::to_value(result)?)
    }

    pub async fn call_mcp_tool(
        &self,
        server: &str,
        tool: &str,
        arguments: Option<serde_json::Value>,
        meta: Option<serde_json::Value>,
    ) -> anyhow::Result<CallToolResult> {
        self.current_mcp_runtime()
            .await
            .manager_arc()
            .call_tool(server, tool, arguments, meta)
            .await
    }

    /// Lists the same direct Codex tools that a model turn would see, flattened for MCP.
    pub async fn list_direct_tools(&self) -> CodexResult<Vec<Tool>> {
        Ok(self
            .direct_tool_entries()
            .await?
            .into_iter()
            .map(|entry| entry.tool)
            .collect())
    }

    /// Calls a direct Codex tool by its flattened MCP name.
    pub async fn call_direct_tool(
        &self,
        name: &str,
        arguments: Option<serde_json::Value>,
    ) -> CodexResult<CallToolResult> {
        let turn_context = self.codex.session.new_default_turn().await;
        let step_context = self
            .codex
            .session
            .capture_step_context(Arc::clone(&turn_context))
            .await;
        let cancellation_token = CancellationToken::new();
        let router = crate::session::turn::built_tools(
            self.codex.session.as_ref(),
            step_context.as_ref(),
            &cancellation_token,
        )
        .await?;
        let entry = self
            .direct_tool_entries_for_router(router.as_ref())
            .into_iter()
            .find(|entry| entry.mcp_name == name)
            .ok_or_else(|| CodexErr::InvalidRequest(format!("unknown tool `{name}`")))?;

        let call_id = format!("mcp-direct-{}", uuid::Uuid::new_v4());
        let payload = match entry.payload_kind {
            DirectToolPayloadKind::Function => {
                let arguments = arguments.unwrap_or_else(|| serde_json::json!({}));
                let arguments = serde_json::to_string(&arguments).map_err(|err| {
                    CodexErr::InvalidRequest(format!("failed to serialize tool arguments: {err}"))
                })?;
                codex_tools::ToolPayload::Function { arguments }
            }
            DirectToolPayloadKind::Custom => {
                let input = arguments
                    .and_then(|value| value.get("input").cloned())
                    .and_then(|value| value.as_str().map(ToString::to_string))
                    .ok_or_else(|| {
                        CodexErr::InvalidRequest(
                            "custom tool arguments must include string field `input`".to_string(),
                        )
                    })?;
                codex_tools::ToolPayload::Custom { input }
            }
            DirectToolPayloadKind::ToolSearch => {
                let arguments = serde_json::from_value(
                    arguments.unwrap_or_else(|| serde_json::json!({})),
                )
                .map_err(|err| {
                    CodexErr::InvalidRequest(format!("failed to parse tool_search arguments: {err}"))
                })?;
                codex_tools::ToolPayload::ToolSearch { arguments }
            }
        };
        let call = crate::tools::router::ToolCall {
            tool_name: entry.tool_name,
            call_id: call_id.clone(),
            payload,
        };
        let tracker = Arc::new(Mutex::new(crate::turn_diff_tracker::TurnDiffTracker::new()));
        let result = router
            .dispatch_tool_call_with_code_mode_result(
                Arc::clone(&self.codex.session),
                step_context,
                cancellation_token,
                tracker,
                call,
                crate::tools::router::ToolCallSource::Direct,
            )
            .await
            .map_err(|err| CodexErr::InvalidRequest(err.to_string()))?;

        Ok(direct_tool_result_to_mcp(result.into_response()))
    }

    async fn direct_tool_entries(&self) -> CodexResult<Vec<DirectToolEntry>> {
        let turn_context = self.codex.session.new_default_turn().await;
        let step_context = self.codex.session.capture_step_context(turn_context).await;
        let cancellation_token = CancellationToken::new();
        let router = crate::session::turn::built_tools(
            self.codex.session.as_ref(),
            step_context.as_ref(),
            &cancellation_token,
        )
        .await?;
        Ok(self.direct_tool_entries_for_router(router.as_ref()))
    }

    fn direct_tool_entries_for_router(
        &self,
        router: &crate::tools::router::ToolRouter,
    ) -> Vec<DirectToolEntry> {
        router
            .model_visible_specs()
            .into_iter()
            .flat_map(direct_tool_entries_from_spec)
            .collect()
    }

    pub fn enabled(&self, feature: Feature) -> bool {
        self.codex.enabled(feature)
    }

    pub async fn increment_out_of_band_elicitation_count(&self) -> CodexResult<u64> {
        let mut guard = self.out_of_band_elicitation_count.lock().await;
        let was_zero = *guard == 0;
        *guard = guard.checked_add(1).ok_or_else(|| {
            CodexErr::Fatal("out-of-band elicitation count overflowed".to_string())
        })?;

        if was_zero {
            self.codex
                .session
                .set_out_of_band_elicitation_pause_state(/*paused*/ true);
        }

        Ok(*guard)
    }

    pub async fn decrement_out_of_band_elicitation_count(&self) -> CodexResult<u64> {
        let mut guard = self.out_of_band_elicitation_count.lock().await;
        if *guard == 0 {
            return Err(CodexErr::InvalidRequest(
                "out-of-band elicitation count is already zero".to_string(),
            ));
        }

        *guard -= 1;
        let now_zero = *guard == 0;
        if now_zero {
            self.codex
                .session
                .set_out_of_band_elicitation_pause_state(/*paused*/ false);
        }

        Ok(*guard)
    }
}

fn direct_tool_entries_from_spec(spec: codex_tools::ToolSpec) -> Vec<DirectToolEntry> {
    match serde_json::to_value(spec) {
        Ok(serde_json::Value::Object(mut value)) => {
            match value.remove("type").and_then(|value| value.as_str().map(str::to_string)) {
                Some(kind) if kind == "function" => function_entry(None, value)
                    .into_iter()
                    .collect(),
                Some(kind) if kind == "custom" => custom_entry(value).into_iter().collect(),
                Some(kind) if kind == "tool_search" => {
                    let description = value
                        .remove("description")
                        .and_then(|value| value.as_str().map(str::to_string));
                    let input_schema = value
                        .remove("parameters")
                        .unwrap_or_else(|| serde_json::json!({"type": "object"}));
                    vec![DirectToolEntry {
                        mcp_name: "tool_search".to_string(),
                        tool_name: codex_tools::ToolName::plain("tool_search"),
                        payload_kind: DirectToolPayloadKind::ToolSearch,
                        tool: Tool {
                            name: "tool_search".to_string(),
                            title: None,
                            description,
                            input_schema,
                            output_schema: None,
                            annotations: None,
                            icons: None,
                            meta: None,
                        },
                    }]
                }
                Some(kind) if kind == "namespace" => namespace_entries(value),
                Some(_) | None => Vec::new(),
            }
        }
        Ok(_) | Err(_) => Vec::new(),
    }
}

fn namespace_entries(mut value: serde_json::Map<String, serde_json::Value>) -> Vec<DirectToolEntry> {
    let Some(namespace) = value
        .remove("name")
        .and_then(|value| value.as_str().map(str::to_string))
    else {
        return Vec::new();
    };
    let Some(serde_json::Value::Array(tools)) = value.remove("tools") else {
        return Vec::new();
    };

    tools
        .into_iter()
        .filter_map(|tool| match tool {
            serde_json::Value::Object(mut tool) => {
                tool.remove("type");
                function_entry(Some(namespace.as_str()), tool)
            }
            _ => None,
        })
        .collect()
}

fn function_entry(
    namespace: Option<&str>,
    mut value: serde_json::Map<String, serde_json::Value>,
) -> Option<DirectToolEntry> {
    let name = value
        .remove("name")
        .and_then(|value| value.as_str().map(str::to_string))?;
    let mut description = value
        .remove("description")
        .and_then(|value| value.as_str().map(str::to_string));
    let mut input_schema = value
        .remove("parameters")
        .unwrap_or_else(|| serde_json::json!({"type": "object"}));
    let mcp_name = namespace.map_or_else(|| name.clone(), |namespace| format!("{namespace}.{name}"));
    sanitize_direct_tool_for_mcp(&mcp_name, &mut description, &mut input_schema);
    let tool_name = namespace.map_or_else(
        || codex_tools::ToolName::plain(name.clone()),
        |namespace| codex_tools::ToolName::namespaced(namespace, name.clone()),
    );

    Some(DirectToolEntry {
        mcp_name: mcp_name.clone(),
        tool_name,
        payload_kind: DirectToolPayloadKind::Function,
        tool: Tool {
            name: mcp_name,
            title: None,
            description,
            input_schema,
            output_schema: None,
            annotations: None,
            icons: None,
            meta: None,
        },
    })
}

fn sanitize_direct_tool_for_mcp(
    mcp_name: &str,
    description: &mut Option<String>,
    input_schema: &mut serde_json::Value,
) {
    if mcp_name != "exec_command" {
        return;
    }

    *description = Some(
        "Runs a local shell command and returns output. Do not use sudo or request escalated permissions; commands run as the local WebCodex user.".to_string(),
    );

    let Some(schema) = input_schema.as_object_mut() else {
        return;
    };
    let Some(properties) = schema
        .get_mut("properties")
        .and_then(serde_json::Value::as_object_mut)
    else {
        return;
    };

    for field in ["justification", "sandbox_permissions", "prefix_rule"] {
        properties.remove(field);
    }
}

fn custom_entry(mut value: serde_json::Map<String, serde_json::Value>) -> Option<DirectToolEntry> {
    let name = value
        .remove("name")
        .and_then(|value| value.as_str().map(str::to_string))?;
    let description = value
        .remove("description")
        .and_then(|value| value.as_str().map(str::to_string));

    Some(DirectToolEntry {
        mcp_name: name.clone(),
        tool_name: codex_tools::ToolName::plain(name.clone()),
        payload_kind: DirectToolPayloadKind::Custom,
        tool: Tool {
            name,
            title: None,
            description,
            input_schema: serde_json::json!({
                "type": "object",
                "properties": {
                    "input": {
                        "type": "string",
                        "description": "Freeform custom tool input."
                    }
                },
                "required": ["input"],
                "additionalProperties": false
            }),
            output_schema: None,
            annotations: None,
            icons: None,
            meta: None,
        },
    })
}

fn direct_tool_result_to_mcp(item: ResponseInputItem) -> CallToolResult {
    match item {
        ResponseInputItem::FunctionCallOutput { output, .. }
        | ResponseInputItem::CustomToolCallOutput { output, .. } => {
            let text = match output.body {
                FunctionCallOutputBody::Text(text) => text,
                FunctionCallOutputBody::ContentItems(items) => {
                    function_call_output_content_items_to_text(&items).unwrap_or_default()
                }
            };
            CallToolResult {
                content: vec![serde_json::json!({"type": "text", "text": text})],
                structured_content: None,
                is_error: output.success.map(|success| !success),
                meta: None,
            }
        }
        ResponseInputItem::ToolSearchOutput { tools, .. } => CallToolResult {
            content: vec![serde_json::json!({
                "type": "text",
                "text": serde_json::to_string(&tools).unwrap_or_default()
            })],
            structured_content: Some(serde_json::Value::Array(tools)),
            is_error: Some(false),
            meta: None,
        },
        other => CallToolResult {
            content: vec![serde_json::json!({
                "type": "text",
                "text": serde_json::to_string(&other).unwrap_or_default()
            })],
            structured_content: None,
            is_error: Some(false),
            meta: None,
        },
    }
}
