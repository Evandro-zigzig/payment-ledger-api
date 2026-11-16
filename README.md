# Payment Ledger API — Idempotent Banking Microservice

 **Parte 1 de 2 da Arquitetura Multi-Região de Disaster Recovery na AWS.**  
 🔗 **Repositório de Infraestrutura, Terraform & GitOps:** [eks-banking-dr-infra](https://github.com)

---

## 1. Problemática & Contexto de Negócio

Em instituições financeiras que processam transferências instantâneas, falhas temporárias de rede ou desvios de tráfego entre regiões (*Disaster Recovery*) fazem com que os clientes reenviem a mesma requisição de pagamento (*retries*). Sem proteção adequada, o banco corre o risco de **debitar o cliente duas vezes** ou perder a rastreabilidade da transação durante o encerramento de contentores.

Este microsserviço foi desenvolvido em **Go (Golang)** para registar transferências de forma idempotente em clusters **Amazon EKS** multi-região (`us-east-1` e `us-west-2`). Atualmente, grava o registo da transferência no DynamoDB; não movimenta saldos nem executa débitos ou créditos.

---

## 2. Decisões Técnicas & Engenharia (Por que construímos assim?)

| Pilar | Decisão Técnica | Justificação de Engenharia |
| :--- | :--- | :--- |
| **Idempotência do Registo** | **Escrita Condicional** (`attribute_not_exists`) | Utiliza uma expressão condicional no Amazon DynamoDB para rejeitar (`409 Conflict`) um segundo registo com o mesmo `transaction_id`. A API não executa débitos ou créditos. |
| **Segurança de Contentor** | **Multi-Stage + Google Distroless (`nonroot`)** | A imagem final (~12 MB) não possui *shell* (`bash`/`sh`) nem gestores de pacotes (`apt`), e corre com utilizador sem privilégios (`UID 65532`), reduzindo a superfície de ataque a zero. |
| **Alta Disponibilidade** | **Graceful Shutdown (`SIGTERM`)** | Interceta sinais de encerramento do Kubernetes e aguarda até 15 segundos para concluir transferências em andamento antes de fechar o servidor HTTP. |
| **Failover Inteligente** | **Deep Health Check (`/healthz`)** | Valida em tempo real a conectividade com a tabela regional do DynamoDB. Caso a região degrade, retorna `503 Service Unavailable`, acionando o failover automático no Route 53. |
| **Observabilidade** | **Prometheus Metrics + JSON Logs (`slog`)** | Expõe métricas de negócio em `/metrics` (`banking_transfers_settled_total`, `banking_transfers_duplicates_blocked_total`, latência) e logs estruturados em JSON. |

---

## 3. Endpoints da API

| Método | Endpoint | Descrição | Códigos HTTP |
| :--- | :--- | :--- | :--- |
| `POST` | `/v1/transfers` | Regista uma transferência de forma idempotente. | `201 Created`, `409 Conflict` (Duplicado), `400 Bad Request`, `413` (Pedido demasiado grande) |
| `GET` | `/healthz` | Verifica a saúde da API e da base de dados na região local. | `200 OK`, `503 Service Unavailable` |
| `GET` | `/metrics` | Expõe métricas de negócio e latência no formato Prometheus. | `200 OK` |

---

## 4. Como Executar e Testar Localmente

### Pré-requisitos
* Go `1.23+` (versão mínima declarada em `go.mod`)
* Docker (opcional, para validar o build seguro)
* Credenciais AWS configuradas localmente (para acesso à tabela `banking-ledger-global`)

### Passo a Passo

1. **Clonar o repositório e instalar dependências:**
   ```bash
   git clone https://github.com/Evandro-zigzig/payment-ledger-api.git
   cd payment-ledger-api
   go mod download
   ```

2. **Executar os testes unitários (não requerem credenciais AWS):**
   ```bash
   go test ./...
   ```

3. **Iniciar a API localmente:**
   ```bash
   export AWS_REGION=us-east-1
   export DYNAMODB_TABLE=banking-ledger-global
   go run ./cmd/api/main.go
   ```

4. **Testar um registo e validar a idempotência:**
   ```bash
   # Primeira chamada: Sucesso (201 Created)
   curl -i -X POST http://localhost:8080/v1/transfers \
     -H "Content-Type: application/json" \
     -d '{"transaction_id":"tx-9901","source_account":"ACC-100","target_account":"ACC-200","amount":250.00,"currency":"USD"}'

   # Segunda chamada com o mesmo ID: Bloqueada (409 Conflict)
   curl -i -X POST http://localhost:8080/v1/transfers \
     -H "Content-Type: application/json" \
     -d '{"transaction_id":"tx-9901","source_account":"ACC-100","target_account":"ACC-200","amount":250.00,"currency":"USD"}'
   ```

5. **Construir a imagem Docker segura (Distroless):**
   ```bash
   docker build -t payment-ledger-api:v1.0.0 .
   ```
