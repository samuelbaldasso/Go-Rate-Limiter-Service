# Golang Rate Limiter

Microsserviço HTTP em Go que implementa rate limiting por cliente usando o
algoritmo **Token Bucket**, construído inteiramente com a biblioteca padrão
(`net/http`, `sync`, `time`, `context`) — sem dependências externas.

## Sumário

- [Visão geral](#visão-geral)
- [Arquitetura](#arquitetura)
- [Estrutura de diretórios](#estrutura-de-diretórios)
- [Configuração (variáveis de ambiente)](#configuração-variáveis-de-ambiente)
- [Como rodar](#como-rodar)
- [Testes](#testes)
- [Comportamento observado](#comportamento-observado)
- [Limitações conhecidas](#limitações-conhecidas)
- [ADRs](#adrs)

## Visão geral

O serviço expõe um handler HTTP protegido por um middleware de rate limiting.
Cada cliente é identificado por IP (com suporte a `X-Forwarded-For`) e recebe
um "balde" de tokens que se esvazia a cada requisição e se reabastece a uma
taxa configurável ao longo do tempo. Quando o balde de um cliente está vazio,
o serviço responde `429 Too Many Requests`; caso contrário, a requisição é
repassada normalmente ao handler final.

O estado de cada cliente (tokens restantes, timestamp da última requisição) é
mantido inteiramente em memória, em um `map[string]*visitor` protegido por
locks — não há dependência de banco de dados ou cache externo.

## Arquitetura

```
                 ┌─────────────────────────┐
  Requisição --> │  RateLimit middleware    │
                 │  (extrai IP do cliente)  │
                 └─────────────┬────────────┘
                               │
                               v
                 ┌─────────────────────────┐
                 │  Limiter.Allow(ip)       │
                 │  - RWMutex global        │
                 │    (busca/cria visitor)  │
                 │  - Mutex por visitor     │
                 │    (matemática de token) │
                 └─────────────┬────────────┘
                               │
                 200 (allow)   │   429 (deny)
                               v
                 ┌─────────────────────────┐
                 │  Handler final (mux)     │
                 └─────────────────────────┘

  Goroutine de cleanup (time.Ticker) roda em paralelo,
  removendo visitantes inativos há mais que o TTL configurado.
```

**Fluxo de uma requisição:**

1. O middleware `RateLimit` extrai o IP do cliente (`internal/middleware/rate_limit.go`).
2. Chama `Limiter.Allow(ip)` (`internal/limiter/bucket.go`), que:
   - Busca o `visitor` correspondente ao IP (lock de leitura no mapa global; lock de escrita apenas se o visitante ainda não existir).
   - Trava o `Mutex` individual do visitante, calcula quantos tokens foram acumulados desde a última requisição (`elapsed * rate`, limitado ao `burst`), e decrementa um token se houver saldo.
3. Se `Allow` retornar `false`, responde `429`. Caso contrário, repassa para o handler seguinte.
4. Em paralelo, uma goroutine de limpeza (`StartCleanup`) varre o mapa periodicamente e remove visitantes inativos há mais que o TTL configurado, evitando vazamento de memória.

## Estrutura de diretórios

```
cmd/api/main.go                    Ponto de entrada: sobe o servidor HTTP,
                                    lê configuração via env vars, registra
                                    rotas e o middleware, trata shutdown
                                    gracioso.

internal/limiter/bucket.go         Lógica de negócio do Token Bucket:
                                    struct Visitor, struct Limiter, Allow(),
                                    StartCleanup().

internal/limiter/bucket_test.go    Testes unitários do limiter (burst,
                                    refill, concorrência, cleanup).

internal/middleware/rate_limit.go  Middleware HTTP que envelopa um
                                    http.Handler e consulta o limiter antes
                                    de repassar a requisição.

internal/middleware/rate_limit_test.go
                                    Testes do middleware via httptest.

Dockerfile                         Build multi-stage (builder golang:alpine
                                    -> imagem final scratch).

docker-compose.yml                 Sobe o serviço localmente com defaults.
```

## Configuração (variáveis de ambiente)

| Variável                      | Default | Descrição                                            |
|--------------------------------|---------|-------------------------------------------------------|
| `PORT`                         | `8080`  | Porta em que o servidor HTTP escuta.                   |
| `RATE_LIMIT_RPS`                | `5`     | Tokens (requisições) reabastecidos por segundo, por cliente. |
| `RATE_LIMIT_BURST`              | `10`    | Capacidade máxima do balde (nº de requisições em rajada permitidas). |
| `RATE_LIMIT_CLEANUP_INTERVAL`   | `1m`    | Intervalo entre varreduras da goroutine de limpeza.    |
| `RATE_LIMIT_VISITOR_TTL`        | `3m`    | Tempo de inatividade após o qual um visitante é removido do mapa. |

## Como rodar

Via Docker Compose (recomendado, não requer Go instalado):

```bash
docker compose up --build
```

Localmente, com Go instalado:

```bash
go run ./cmd/api
```

O serviço estará disponível em `http://localhost:8080`.

## Testes

```bash
go vet ./...
go test -race ./...
```

Ou via Docker, sem precisar instalar Go:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.22-alpine \
  sh -c "go vet ./... && go test -race ./..."
```

Cobertura atual:
- `internal/limiter`: burst e negação após esgotamento, reabastecimento ao longo do tempo, independência entre chaves distintas, concorrência na mesma chave (`-race`), remoção de visitantes inativos.
- `internal/middleware`: resposta `429` após esgotar o burst, isolamento de clientes via `X-Forwarded-For`, fallback de extração de IP para `RemoteAddr`.

Não coberto (fora de escopo desta entrega): testes de carga/benchmark e testes de integração via container real.

## Comportamento observado

Com os defaults (`RATE_LIMIT_RPS=5`, `RATE_LIMIT_BURST=10`), 12 requisições em sequência imediata resultam em:

```
request 1..10: 200
request 11:    429
request 12:    429
```

Após ~1s de espera, uma nova requisição volta a ser aceita (`200`), pois o balde é reabastecido a 5 tokens/segundo.

## Limitações conhecidas

- **Estado não compartilhado entre réplicas**: o mapa de visitantes vive na memória do processo. Rodar múltiplas réplicas do serviço atrás de um load balancer resulta em limites efetivos multiplicados pelo número de réplicas (cada uma mantém seu próprio balde por cliente). Para rate limiting distribuído corretamente, seria necessário um store compartilhado (ex.: Redis) — fora do escopo deste exercício, que pede explicitamente estado em memória.
- **`X-Forwarded-For` confiado sem validação de proxy**: o serviço usa o primeiro IP do header `X-Forwarded-For` quando presente, sem validar que a requisição de fato veio de um proxy confiável. Um cliente malicioso com acesso direto ao serviço pode forjar esse header e contornar o limite por IP. Aceitável para este exercício; requer um proxy reverso confiável na frente (que sobrescreva/valide o header) antes de uso em produção exposta à internet.
- **Imagem final em `scratch`**: não contém `ca-certificates` nem shell. Suficiente para este serviço (não faz chamadas HTTPS externas), mas exigiria ajuste (`ca-certificates` ou base `alpine`) se o serviço vier a fazer chamadas para APIs externas via TLS.

## ADRs

### ADR-001: Token Bucket como algoritmo de rate limiting

**Status:** Aceito

**Contexto:** É necessário escolher um algoritmo de rate limiting por cliente. As opções consideradas foram Token Bucket, Fixed Window Counter e Sliding Window Log/Counter.

**Decisão:** Usar Token Bucket, com implementação manual (sem `golang.org/x/time/rate`), calculando tokens acumulados sob demanda a partir do `time.Duration` decorrido desde a última requisição — sem necessidade de goroutine dedicada por cliente para "encher" o balde.

**Alternativas descartadas:**
- *Fixed Window Counter*: mais simples, mas permite picos de até 2x o limite na borda entre janelas (ex.: rajada no fim de uma janela + rajada no início da próxima).
- *Sliding Window Log*: mais preciso, mas exigiria armazenar timestamps de cada requisição por cliente, aumentando uso de memória e complexidade sem necessidade real para este caso de uso.
- *`golang.org/x/time/rate`*: implementa Token Bucket de forma robusta e testada, mas foi descartado porque o requisito explícito era usar apenas a biblioteca padrão do Go (`sync`, `time`) — o objetivo é também didático, para exercitar a lógica de concorrência manualmente.

**Consequências:** A lógica de refill é *lazy* (calculada no momento da requisição, não por um timer contínuo por cliente), o que é mais eficiente em memória e CPU para um número grande de clientes esparsos, mas exige cuidado para não haver overflow do balde acima do `burst` configurado (mitigado com `if v.tokens > l.burst { v.tokens = l.burst }`).

---

### ADR-002: Granularidade de locks — RWMutex global + Mutex por visitante

**Status:** Aceito

**Contexto:** Múltiplas goroutines podem acessar e modificar o estado de rate limiting concorrentemente — tanto para clientes diferentes quanto, ocasionalmente, para o mesmo cliente (requisições simultâneas do mesmo IP).

**Decisão:** Usar dois níveis de lock:
1. Um `sync.RWMutex` no `Limiter`, usado com `RLock` para leitura (caminho comum: visitante já existe) e `Lock` apenas na criação de um novo visitante (com *double-checked locking* para evitar condição de corrida entre o `RUnlock` e o `Lock` de escrita).
2. Um `sync.Mutex` individual dentro de cada `visitor`, protegendo apenas o cálculo de tokens daquele cliente específico.

**Alternativas descartadas:**
- *Um único Mutex global* protegendo leitura, criação e matemática de tokens: mais simples, porém serializaria **todas** as requisições do serviço em um único lock, mesmo entre clientes completamente independentes — um gargalo severo sob carga.
- *`sync.Map`*: elimina a necessidade do `RWMutex` global para leitura/escrita concorrente do mapa, mas foi descartado porque `sync.Map` é otimizado para chaves relativamente estáveis com poucas escritas e leituras predominantes de chaves distintas — nosso padrão de acesso (leitura frequente da mesma chave, mais explícito com `RWMutex`+map comum) é mais previsível e fácil de raciocinar sobre correção neste contexto didático.

**Consequências:** Requisições para clientes diferentes não competem por lock além do brevíssimo `RLock` de busca no mapa. Requisições simultâneas para o **mesmo** cliente são serializadas apenas entre si (via o `Mutex` do visitante), o que é o comportamento correto e esperado (a matemática de tokens não pode ser paralela para o mesmo estado).

---

### ADR-003: Limpeza de visitantes inativos via goroutine com `time.Ticker`

**Status:** Aceito

**Contexto:** O mapa de visitantes cresce a cada novo IP visto e nunca encolhe sozinho — em Go, mapas não expiram chaves automaticamente. Sem limpeza, o serviço vazaria memória indefinidamente em produção.

**Decisão:** Rodar uma goroutine de background (`Limiter.StartCleanup`), disparada a partir de `main.go`, que usa um `time.Ticker` para varrer o mapa periodicamente (`RATE_LIMIT_CLEANUP_INTERVAL`) e remover visitantes cujo `lastSeen` esteja além do TTL configurado (`RATE_LIMIT_VISITOR_TTL`). A goroutine recebe um `context.Context` e encerra de forma limpa quando o contexto é cancelado (shutdown do serviço).

**Alternativas descartadas:**
- *TTL por entrada com timer individual (`time.AfterFunc` por visitante)*: mais preciso no tempo de expiração, mas cria overhead de um timer por cliente ativo — desnecessário para o caso de uso, e mais complexo de cancelar corretamente quando um visitante volta a ficar ativo antes de expirar.
- *Sem limpeza (aceitar o leak)*: descartado por ser uma falha de design conhecida e evitável, citada explicitamente no `task.md` como requisito.

**Consequências:** Existe uma janela entre a inatividade real de um cliente e sua remoção efetiva do mapa (no máximo `TTL + CLEANUP_INTERVAL`), o que é aceitável — o objetivo é limitar o crescimento do mapa, não expirar imediatamente.

---

### ADR-004: Extração de IP com suporte a `X-Forwarded-For`, sem validação de proxy confiável

**Status:** Aceito, com ressalva documentada

**Contexto:** Em produção, o serviço tipicamente roda atrás de um proxy reverso ou load balancer, que reescreve `RemoteAddr` para o IP do proxy, não do cliente real. Sem suporte a `X-Forwarded-For`, o rate limiting acabaria agrupando todos os clientes sob o IP do proxy.

**Decisão:** Se o header `X-Forwarded-For` estiver presente, usar o primeiro IP da lista como chave; caso contrário, usar `RemoteAddr`.

**Alternativas descartadas:**
- *Ignorar `X-Forwarded-For` e usar sempre `RemoteAddr`*: mais seguro contra spoofing, mas inutiliza o rate limiting por cliente real em qualquer topologia com proxy/load balancer na frente — cenário comum em produção.
- *Implementar uma lista de proxies confiáveis e validar a cadeia de `X-Forwarded-For`*: mais correto e seguro, porém adiciona complexidade de configuração (lista de CIDRs confiáveis) fora do escopo deste exercício.

**Consequências:** Um cliente com acesso direto ao serviço (sem passar por um proxy confiável na frente) pode forjar o header `X-Forwarded-For` e contornar o rate limiting associado ao seu IP real. Este é um trade-off consciente, documentado na seção [Limitações conhecidas](#limitações-conhecidas); antes de expor o serviço diretamente à internet sem um proxy confiável, esta lógica deve ser revisada.

---

### ADR-005: Shutdown gracioso com `signal.NotifyContext`

**Status:** Aceito

**Contexto:** Sem tratamento de sinais, um `SIGTERM` (por exemplo, ao parar um container) mataria o processo abruptamente, cortando conexões em andamento e deixando a goroutine de limpeza órfã até o processo morrer.

**Decisão:** Usar `signal.NotifyContext` para capturar `os.Interrupt` e `syscall.SIGTERM`, propagar esse `context.Context` para a goroutine de cleanup (que encerra via `ctx.Done()`), e chamar `srv.Shutdown(shutdownCtx)` com timeout de 5 segundos ao receber o sinal.

**Alternativas descartadas:**
- *Sem tratamento de shutdown*: mais simples, mas não foi considerado aceitável para um serviço destinado a rodar em containers/orquestradores, onde `SIGTERM` é o mecanismo padrão de parada.

**Consequências:** Requisições em andamento no momento do `SIGTERM` têm até 5 segundos para completar antes do processo encerrar; a goroutine de limpeza para de forma determinística junto com o shutdown do servidor, sem vazar goroutines.

---

### ADR-006: Imagem Docker final em `scratch`

**Status:** Aceito, com ressalva documentada

**Contexto:** É necessário empacotar o serviço em uma imagem Docker para deploy. O binário Go é compilado estaticamente (`CGO_ENABLED=0`), o que permite usar uma imagem final mínima.

**Decisão:** Build multi-stage: `golang:1.22-alpine` para compilar, `scratch` como imagem final, copiando apenas o binário.

**Alternativas descartadas:**
- *Imagem final `alpine`*: inclui shell e gerenciador de pacotes, útil para debug (`docker exec` interativo) e already trazendo `ca-certificates`, mas aumenta a superfície de ataque e o tamanho da imagem sem necessidade, já que o serviço não faz chamadas HTTPS externas nem precisa de shell em runtime.

**Consequências:** Imagem final mínima (poucos MBs) e com superfície de ataque reduzida (sem shell, sem gerenciador de pacotes, sem libc dinâmica). Se o serviço vier a precisar fazer chamadas HTTPS para serviços externos, será necessário adicionar `ca-certificates` (copiando de `/etc/ssl/certs/ca-certificates.crt` do estágio de build, ou trocando a imagem final para `alpine`). Debugging em produção fica mais difícil (sem shell para `exec` no container) — mitigado via logs estruturados no `stdout`.
