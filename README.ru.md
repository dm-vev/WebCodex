# WebCodex

WebCodex предоставляет агентские возможности Codex внутри ChatGPT Web.

**Лимиты Codex при этом не расходуются: работа идет через ChatGPT Web, поэтому используется только лимит текущего чата, а опыт остается близким к Codex.**

Проект рассчитан на схему, где ChatGPT получает доступ к локальному окружению через публичную точку MCP, а рабочая машина держит исходящее соединение с публичным шлюзом.

English version: [README.md](README.md)

## Что дает WebCodex

WebCodex использует полноценный Codex как основу, а не повторяет инструменты агента внутри шлюза. ChatGPT Web получает прямые инструменты MCP, работающие через оригинальный код Codex, поэтому выполнение инструментов сохраняет то же окружение, поведение и производительность, что и обычный путь агента Codex. Старые обертки `codex` и `codex-reply` по умолчанию не публикуются.

Публичный сервер сам не выполняет локальные команды. Он принимает MCP-запросы от ChatGPT, пересылает их через подключенный поток локального агента и возвращает MCP-ответ, полученный с локальной машины.

## Архитектура

Система состоит из двух исполняемых файлов.

`webcodex-gate` работает на публичном хосте. Он обслуживает MCP-точку для ChatGPT, служебные OAuth-точки и закрытые точки для потока агента.

`webcodex-agent` работает на машине с рабочим окружением. Он запускает исправленный Codex MCP server через стандартный ввод/вывод, открывает исходящий поток к шлюзу и выполняет пересланные MCP-запросы локально.

```text
ChatGPT Web
  -> публичная HTTPS-точка MCP
  -> webcodex-gate
  -> исходящий поток агента
  -> webcodex-agent
  -> исправленный Codex MCP server
  -> локальная файловая система / командная оболочка
```

## Модель безопасности

WebCodex можно развернуть с разными уровнями доступа. Развертывание с полным доступом может открывать файловые инструменты, запуск команд, применение патча и команды с правами root. Ограниченное развертывание может оставить только явно разрешенные инструменты MCP или запретить отдельные инструменты на шлюзе.

Аутентификация разделена на два токена:

| Переменная | Кто использует | Назначение |
| --- | --- | --- |
| `WEBCODEX_PUBLIC_TOKEN` | ChatGPT / MCP-клиенты | Bearer-токен, который выдается OAuth-шлюзом и принимается `/mcp` |
| `WEBCODEX_AGENT_TOKEN` | локальный агент | Bearer-токен для `/agent/stream` и `/agent/result` |
| `WEBCODEX_OAUTH_CLIENT_ID` | настройка OAuth в ChatGPT | необязательная проверка идентификатора клиента |
| `WEBCODEX_OAUTH_CLIENT_SECRET` | настройка OAuth в ChatGPT | необязательная проверка секрета клиента |

Секреты должны находиться в окружении развертывания, а не в git.

Доступные инструменты настраиваются на шлюзе:

| Переменная | Поведение |
| --- | --- |
| `WEBCODEX_ALLOWED_TOOLS` | Список разрешенных инструментов через запятую. Пустое значение разрешает все инструменты Codex, кроме явно запрещенных. |
| `WEBCODEX_DENIED_TOOLS` | Список запрещенных инструментов через запятую. Запрет имеет приоритет над разрешением. |

Политика применяется и к `tools/list`, и к `tools/call`, поэтому скрытые инструменты нельзя вызвать напрямую по имени через публичную MCP-точку.

## Подключение ChatGPT

Используется пользовательский MCP-коннектор с OAuth-аутентификацией.

| Поле | Значение |
| --- | --- |
| Server URL | `https://<mcp-host>/mcp` |
| Authorization URL | `https://<mcp-host>/oauth/authorize` |
| Token URL | `https://<mcp-host>/oauth/token` |
| Client ID | значение `WEBCODEX_OAUTH_CLIENT_ID` |
| Client Secret | значение `WEBCODEX_OAUTH_CLIENT_SECRET` |
| Token endpoint auth | `client_secret_basic` или `client_secret_post` |

Реализация OAuth здесь минимальная: она проверяет настроенные учетные данные клиента и выдает настроенный публичный Bearer-токен для MCP-точки.

## Конфигурация шлюза

`webcodex-gate` настраивается через переменные окружения.

| Переменная | Обязательная | По умолчанию | Описание |
| --- | --- | --- | --- |
| `WEBCODEX_ADDR` | нет | `:8080` | адрес HTTP-сервера |
| `WEBCODEX_PUBLIC_URL` | рекомендуется | из `WEBCODEX_ADDR` | публичный HTTPS-адрес для метаданных OAuth |
| `WEBCODEX_PUBLIC_TOKEN` | да | нет | Bearer-токен для `/mcp` |
| `WEBCODEX_AGENT_TOKEN` | да | нет | Bearer-токен для точек локального агента |
| `WEBCODEX_OAUTH_CLIENT_ID` | нет | нет | разрешенный идентификатор OAuth-клиента |
| `WEBCODEX_OAUTH_CLIENT_SECRET` | нет | нет | разрешенный секрет OAuth-клиента |
| `WEBCODEX_CALL_TIMEOUT` | нет | `2m` | максимальное время одного пересланного MCP-вызова |
| `WEBCODEX_ALLOWED_TOOLS` | нет | нет | список разрешенных MCP-инструментов через запятую |
| `WEBCODEX_DENIED_TOOLS` | нет | нет | список запрещенных MCP-инструментов через запятую |

Публичный обратный прокси должен прокидывать эти пути в шлюз:

| Путь | Назначение |
| --- | --- |
| `/mcp` | точка MCP JSON-RPC для ChatGPT |
| `/.well-known/oauth-protected-resource` | метаданные для MCP-аутентификации |
| `/.well-known/oauth-authorization-server` | метаданные OAuth-сервера |
| `/oauth/authorize` | точка авторизации OAuth |
| `/oauth/token` | точка выдачи OAuth-токена |
| `/agent/stream` | закрытый поток запросов локального агента |
| `/agent/result` | закрытая точка ответов локального агента |

TLS обычно завершается на обратном прокси.

## Конфигурация агента

`webcodex-agent` настраивается через переменные окружения.

| Переменная | Обязательная | По умолчанию | Описание |
| --- | --- | --- | --- |
| `WEBCODEX_GATE_URL` | да | нет | публичный базовый URL шлюза |
| `WEBCODEX_AGENT_TOKEN` | да | нет | общий закрытый токен для точек агента |
| `WEBCODEX_CODEX_MCP_CMD` | нет | `third_party/codex/codex-rs/target/debug/codex-mcp-server` | shell-команда запуска исправленного Codex MCP server |
| `WEBCODEX_MCP_CALL_TIMEOUT` | нет | `2m` | максимальное время одного локального MCP-запроса |

Процесс агента должен работать под пользователем, которому принадлежит нужное окружение Codex. Для полного локального контроля этому пользователю нужна конфигурация Codex такого вида:

```toml
approval_policy = "never"
sandbox_mode = "danger-full-access"
```

Sudo без пароля опционален и нужен только для развертываний, где ожидаются инструменты с правами root.

## Исправленный Codex

Исправленная копия исходников Codex завендорена в `third_party/codex`.

Патч MCP меняет `codex-mcp-server` так, чтобы сервер публиковал прямые внутренние инструменты Codex как отдельные MCP-инструменты. Главные точки изменения:

```text
third_party/codex/codex-rs/core/src/codex_thread.rs
third_party/codex/codex-rs/mcp-server/src/message_processor.rs
```

`CODEX_MCP_LEGACY_TOOLS=1` возвращает старые обертки `codex` и `codex-reply`.

## Сборка

Требования:

| Компонент | Для чего нужен |
| --- | --- |
| Go | `webcodex-gate` и `webcodex-agent` |
| инструменты Rust | исправленный `codex-mcp-server` |

Сборка локальных исполняемых файлов:

```bash
go build -o bin/webcodex-gate ./cmd/gate
go build -o bin/webcodex-agent ./cmd/agent
(cd third_party/codex/codex-rs && cargo build -p codex-mcp-server)
```

Сборка шлюза для другой Linux-архитектуры:

```bash
GOOS=linux GOARCH=arm64 go build -o bin/webcodex-gate-linux-arm64 ./cmd/gate
```

## Схема развертывания

Типичное развертывание:

| Хост | Процесс | Сеть |
| --- | --- | --- |
| публичный сервер | `webcodex-gate` за HTTPS-обратным прокси | принимает подключения ChatGPT и агента |
| рабочая машина | `webcodex-agent` под systemd | открывает исходящее HTTPS-соединение к шлюзу |

Примеры unit-файлов systemd лежат в `deploy/`. Они ожидают файлы окружения:

```text
/etc/webcodex/gate.env
/etc/webcodex/agent.env
```

## Проверка

Для развертываний, где разрешены команды с повышенными правами, `exec_command` проверяет пользователя локального выполнения и работу sudo:

```bash
curl -sS https://<mcp-host>/mcp \
  -H "Authorization: Bearer $WEBCODEX_PUBLIC_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"exec_command","arguments":{"cmd":"id && sudo -n id","workdir":"/tmp","yield_time_ms":1000,"max_output_tokens":2000}}}' | jq -r '.result.content[0].text'
```

## Проверки разработки

```bash
go test ./...
go vet ./...
(cd third_party/codex/codex-rs && cargo check -p codex-mcp-server)
```
