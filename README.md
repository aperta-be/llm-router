# llm-router

An OpenAI-compatible API proxy that classifies incoming requests and routes them to the most appropriate local [Ollama](https://ollama.com) model. A small classifier model reads each prompt and decides whether it is a `thinking`, `coding`, `simple`, or `general` task — then forwards the request to the model configured for that category.

Comes with a web-based admin panel for configuration, request history, API key management, and benchmarking.

---

## How It Works

```
Client → POST /v1/chat/completions
           │
           ▼
    Classifier model
    (fast, small LLM)
           │
    ┌──────┴───────┐
    │  cache hit?  │  ← SHA-256 keyed TTL cache
    └──────┬───────┘
           │
    classify: thinking / coding / simple / general
           │
    ┌──────┴───────────────────────────┐
    │  thinking  coding  simple  other │
    └──────┬──────┬──────┬─────┬──────┘
           ▼      ▼      ▼     ▼
         model  model  model  default model  (Ollama)
           │
    stream / non-stream response → Client
```

Response headers expose the routing decision:
- `X-Router-Classification: thinking`
- `X-Router-Model: glm-4.7-flash:q8_0`
- `X-Request-ID: <uuid>`

---

## Quick Start (Docker)

**Prerequisites:** Docker, Ollama running locally (or use docker-compose to run both).

```bash
# With docker-compose (starts Ollama + llm-router)
docker compose up -d

# Or run the router container only (Ollama already running)
docker run -d \
  -p 8080:8080 \
  -v llmr_data:/data \
  -e OLLAMA_BASE_URL=http://host.docker.internal:11434 \
  -e ADMIN_PASSWORD=changeme \
  ghcr.io/aperta-be/llm-router:latest
```

The server starts on `:8080`. Admin panel at `http://localhost:8080/admin` (default credentials: `admin` / `changeme`).

---

## Quick Start (Go)

**Prerequisites:** Go 1.25+, Ollama running locally.

```bash
git clone https://github.com/aperta-be/llm-router
cd llm-router
go build -o llm-router .
./llm-router
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP listen port |
| `OLLAMA_BASE_URL` | `http://0.0.0.0:11434` | Ollama base URL |
| `DB_PATH` | `router.db` | SQLite database path |
| `ADMIN_USERNAME` | `admin` | Admin panel username |
| `ADMIN_PASSWORD` | `admin` | Admin panel password |

Model assignments and all other settings are configured from the admin panel and persisted in SQLite.

---

## API

The router is a drop-in replacement for the OpenAI API — point any OpenAI-compatible client at it.

### `POST /v1/chat/completions`

Standard OpenAI chat completions. Set `model` to anything (e.g. `"auto"`) — the router overrides it based on classification.

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer llmr_..." \
  -d '{
    "model": "auto",
    "messages": [{"role": "user", "content": "Write a binary search in Python"}]
  }'
```

Supports streaming (`"stream": true`).

### `POST /v1/classify`

Classify a prompt without calling any generation model. Useful for debugging routing decisions and benchmarking.

```bash
curl http://localhost:8080/v1/classify \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer llmr_..." \
  -d '{
    "model": "auto",
    "messages": [{"role": "user", "content": "Implement a red-black tree"}]
  }'
```

Response:
```json
{
  "classification": "coding",
  "model": "qwen3-coder:latest",
  "cache_hit": false,
  "latency_ms": 312
}
```

### `GET /health`

Returns `{"status": "ok"}`.

### `GET /models`

Returns the currently configured model for each role.

---

## Admin Panel

| Page | URL | Description |
|------|-----|-------------|
| Dashboard | `/admin/dashboard` | Request stats with 1h / 24h / 7d / 30d / all-time filter |
| Config | `/admin/config` | Ollama URL, model assignments, classification prompt, cache settings. Includes a **Test Connection** button that pings Ollama and shows which configured models are available |
| API Keys | `/admin/keys` | Create / revoke API keys with optional expiry (7d / 30d / 90d / 1y) |
| Prompt History | `/admin/prompts` | Search, filter by classification / model, paginate, export to CSV or JSON |
| Users | `/admin/users` | Create users, assign roles (admin / user), enable or disable accounts |

---

## User Management

The router supports multiple users with two roles:

| Role | Access |
|------|--------|
| `admin` | Full admin panel access: config, dashboard, prompt history, user management |
| `user` | Self-service only: create and revoke their own API keys |

**Creating users:** Admins can create additional accounts from `/admin/users`. Users can be enabled or disabled without deletion.

**Self-service API keys:** Non-admin users log in at `/admin/login` and can manage their own API keys at `/admin/keys`. They have no access to config, dashboard, or other users' keys.

**First-run:** A default admin account is created from `ADMIN_USERNAME` / `ADMIN_PASSWORD` on startup.

---

## API Key Authentication

When at least one active API key exists, all `/v1/` endpoints require a `Bearer` token:

```
Authorization: Bearer llmr_<key>
```

Keys are created in the admin panel. Until the first key is created the API is open (useful for initial setup).

Keys support optional expiry and are automatically rejected once expired.

---

## Benchmark Tool

```bash
go run ./cmd/bench [--url http://localhost:8080] [--key llmr_...]
# or set LLMR_API_KEY env var

# Example output:
# Cold (classifier called)  accuracy=24/25 (96%)  avg=318ms  p50=290ms  p95=680ms  p99=820ms  min=110ms  max=950ms
# Warm (cache hit)          accuracy=24/25 (96%)  avg=4ms    p50=3ms    p95=9ms    p99=12ms   min=2ms    max=15ms
```

Runs 25 labelled prompts twice (cold + warm/cached), reports accuracy per category and latency percentiles (avg, p50, p95, p99, min, max).

---

## Security Notes

- Change `ADMIN_USERNAME` / `ADMIN_PASSWORD` before exposing to a network.
- Login is brute-force protected: 5 failed attempts trigger a 15-minute lockout (tracked per username and IP).
- Session cookies are `HttpOnly`.
- API keys are stored as SHA-256 hashes — the raw key is shown only once at creation time.
