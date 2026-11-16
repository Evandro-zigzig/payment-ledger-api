# NovaBanco — API de transferências

API Go para aplicar transferências entre contas com idempotência. O código separa o processo HTTP, as regras do ledger e o adaptador DynamoDB para que cada camada possa ser testada sem credenciais AWS.

> **Âmbito funcional:** a API debita a conta de origem, credita a conta de destino e grava o registo da transferência numa única transação DynamoDB. A primeira versão aceita apenas USD; `amount_minor` representa centavos (por exemplo, USD 250,00 = `25000`).

## Requisitos de negócio e decisões

| Problema do enunciado | Decisão nesta API | Como será verificado |
|---|---|---|
| Falha regional | A API recebe a região por `AWS_REGION`; a infraestrutura executará warm standby ativo-passivo entre `us-east-1` e `us-west-2`. | Game Day no repositório de infraestrutura, medindo RTO e RPO; ainda não executado. |
| Retries duplicados | A API usa `transaction_id` como chave idempotente. A escrita do ledger, o débito e o crédito são uma transação DynamoDB com escrita condicional. | Testes unitários verificam três operações atómicas e classificam duplicados. O teste contra DynamoDB real fica para integração. |
| Encerramento durante atualização | O servidor trata SIGINT/SIGTERM e aguarda até 20 segundos pelos pedidos HTTP em curso. `/livez` verifica apenas o processo; `/readyz` verifica se a instância pode aceitar escritas. | Teste de encerramento e, posteriormente, rolling update sob carga. |

O pressuposto inicial de carga do projeto é **3 000 pedidos/minuto = 50 pedidos/segundo**, incluindo retries. O mesmo valor deve ser usado no dimensionamento, no teste de carga e no Game Day. É uma hipótese de projeto, não uma medição do NovaBanco.

## Estrutura

```text
cmd/api/                 Arranque, configuração AWS e ciclo de vida do servidor
internal/httpapi/        Rotas HTTP, validação do JSON e códigos HTTP
internal/ledger/         Modelo, regras, interface de persistência e adaptador DynamoDB
api/openapi.yaml         Contrato público da API
```

### Impacto da separação

- `cmd/api` é o ponto de composição: cria o cliente AWS, liga a implementação DynamoDB ao serviço e inicia o servidor. Não contém regras de ledger.
- `internal/ledger` é o limite interno do domínio contabilístico. Define transferências e validações e expõe uma interface de armazenamento. O adaptador DynamoDB implementa essa interface.
- `internal/httpapi` converte HTTP em chamadas do serviço e traduz os erros do domínio para respostas HTTP. Não conhece expressões DynamoDB nem as chaves da tabela.
- `api/openapi.yaml` documenta o contrato que os clientes e os testes de contrato podem usar.

O diretório Go `internal` impede que outros módulos importem estes pacotes como API pública. A separação facilita testes unitários sem AWS e reduz o impacto de trocar a persistência ou o transporte HTTP. Não cria, por si só, novos microserviços: continua a existir um único binário e uma única unidade de deploy.

## Modelo de dados DynamoDB

A tabela usa chave composta `PK` (partition key) e `SK` (sort key):

| Item | `PK` | `SK` | Campos usados pela API |
|---|---|---|---|
| Saldo de conta | `ACCOUNT#<account_id>` | `BALANCE` | `balance_minor` (Number), `currency` (String), `updated_at` (String) |
| Transferência | `TRANSFER#<transaction_id>` | `META` | origem, destino, `amount_minor`, moeda, estado, região e data |

Cada transferência usa `TransactWriteItems` com três ações: atualizar a origem apenas se tiver saldo e moeda compatíveis; atualizar um destino existente com a mesma moeda; criar o item de transferência apenas se a chave ainda não existir. Uma falha em qualquer condição cancela as três ações.

**Importante para a infraestrutura:** a tabela antiga com `TransactionID` como chave não é compatível com este modelo. O DynamoDB não permite alterar a chave de uma tabela existente; será necessário criar uma tabela com `PK`/`SK` e migrar/recriar dados de teste na fase de infraestrutura.

Global Tables replicam entre regiões de forma assíncrona no modo assumido. A condição de idempotência protege retries quando o item já está presente na região que recebe o pedido; não garante RPO zero se a região primária falhar antes da replicação. A estratégia de failover deve manter um único writer, controlar a promoção e reconciliar dados. O objetivo de RPO é uma meta a medir, não uma garantia.

## Contrato HTTP

O contrato é mantido em [`api/openapi.yaml`](api/openapi.yaml).

| Método | Endpoint | Resultado |
|---|---|---|
| `POST` | `/v1/transfers` | `201` criada; `400` pedido inválido; `409` duplicada; `422` saldo/conta/moeda incompatível; `503` falha temporária de persistência |
| `GET` | `/livez` | `200` se o processo HTTP estiver ativo; não consulta o DynamoDB |
| `GET` | `/readyz` | `200` se as escritas estiverem ativas e a tabela acessível; `503` em standby ou falha da tabela |
| `GET` | `/healthz` | Alias legado de `/readyz` |
| `GET` | `/metrics` | Métricas Prometheus |

Exemplo de pedido:

```json
{
  "transaction_id": "tx-9901",
  "source_account": "ACC-100",
  "target_account": "ACC-200",
  "amount_minor": 25000,
  "currency": "USD"
}
```

Repetir o pedido com o mesmo `transaction_id` devolve `409 Conflict` e não repete o débito nem o crédito **na região que já conhece esse registo**.

## Executar e testar localmente

É necessário Go 1.23 ou superior. Confirma a versão com `go version`. Os testes unitários usam doubles em memória e não precisam de credenciais AWS:

```bash
go test ./...
go vet ./...
```

Para executar a API contra uma tabela DynamoDB já criada e com contas previamente provisionadas, configurar a cadeia de credenciais AWS local e definir:

```bash
export AWS_REGION=us-east-1
export DYNAMODB_TABLE=novabanco-ledger
export WRITES_ENABLED=true
go run ./cmd/api
```

`WRITES_ENABLED` tem de ser definido explicitamente como `true` ou `false`. A região primária inicia com escrita ativa; a região de standby deve iniciar com escrita desativada. Ativar a região DR exige primeiro confirmar que a primária foi desativada ou efetivamente isolada. A variável é uma proteção de aplicação, não uma garantia de fencing contra uma primária isolada que ainda esteja a aceitar tráfego.

Os testes locais não precisam destas variáveis nem de uma tabela DynamoDB. A execução da API precisa de credenciais AWS fornecidas pela cadeia de credenciais suportada pelo SDK e de uma tabela com as chaves `PK`/`SK` descritas acima. As contas de origem e destino também têm de estar provisionadas antes do pedido.

Exemplo de chamada:

```bash
curl -i -X POST http://localhost:8080/v1/transfers \
  -H "Content-Type: application/json" \
  -d '{"transaction_id":"tx-9901","source_account":"ACC-100","target_account":"ACC-200","amount_minor":25000,"currency":"USD"}'
```

`go run` falha se `DYNAMODB_TABLE` não estiver definida ou se a configuração AWS não puder ser carregada. Não colocar chaves AWS no código nem em `.env` versionado. Em EKS, a identidade da aplicação será fornecida por IRSA; as permissões devem limitar-se a `dynamodb:TransactWriteItems` e `dynamodb:DescribeTable` sobre a tabela necessária.

## Limitações e próximos passos

- Esta versão não autentica nem autoriza clientes. Não a exponhas à Internet nem a uses com contas reais.
- Só aceita USD e não implementa câmbio, criação de contas ou consulta do histórico.
- `/readyz` verifica acesso à tabela e se a região está autorizada a escrever; não prova por si só que todas as operações de negócio funcionarão. Usa `/livez` como liveness para não reiniciar contentores por uma falha temporária do DynamoDB.
- A proteção idempotente é regional durante o período de replicação. O failover ativo-passivo, a reconciliação, o RTO/RPO e o rolling update ainda precisam de provas nos Game Days.
- A aplicação e a infraestrutura vivem em repositórios distintos: este repositório contém código, contrato, testes e CI; Terraform, EKS, IAM, Global Tables, DNS e GitOps pertencem ao repositório de infraestrutura.
