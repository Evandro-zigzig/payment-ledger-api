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
Dockerfile               Imagem multi-stage, compilada e executada sem root
.dockerignore            Limita o contexto enviado ao Docker
Makefile                 Comandos locais comuns de validação e build
.github/workflows/ci.yml CI de pull requests e da branch main
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

## Contentor local

O `Dockerfile` transforma o código Go num binário estático numa fase de compilação e copia apenas esse binário e os certificados CA para uma imagem Distroless sem shell, executada como `nonroot`. Isto reduz o conteúdo da imagem final e evita executar a API como root. O `TARGETOS`/`TARGETARCH` permite ao BuildKit compilar para a plataforma alvo. As caches BuildKit aceleram compilações repetidas, sem fazer parte da imagem.

O `.dockerignore` impede que o contexto inclua `.git`, credenciais em ficheiros `.env`, documentação, artefactos locais e o workflow. O código de `cmd/` e `internal/` continua incluído; não se usa um padrão genérico `api` que pudesse excluir código da aplicação.

Em Linux ou WSL2, com Docker instalado e o daemon ativo:

```bash
docker build --tag novabanco-payment-api:local .
docker image inspect --format '{{.Config.User}}' novabanco-payment-api:local
```

Resultado esperado: o build termina com sucesso e a inspeção indica `nonroot:nonroot`. Se o comando não ligar ao daemon, inicia o Docker Desktop/Engine e confirma `docker info`; se o build falhar a descarregar módulos, confirma o acesso à rede e repete para aproveitar a cache. O contentor da API não é um teste autónomo: exige `DYNAMODB_TABLE`, `WRITES_ENABLED`, configuração AWS e uma tabela compatível. Não iniciar uma API que escreva numa tabela partilhada sem confirmar previamente o ambiente e as contas de teste.

## Comandos locais e validação contínua

O `Makefile` evita que cada pessoa ou pipeline tenha de memorizar comandos diferentes:

| Comando (Linux/WSL2) | O que verifica/faz | Resultado esperado; se falhar |
|---|---|---|
| `make fmt` | Aplica `gofmt` aos ficheiros Go em `cmd/` e `internal/`. | Não imprime ficheiros; rever o diff antes de continuar. |
| `make fmt-check` | Verifica formatação sem modificar ficheiros. | Código de saída 0; se falhar, executar `make fmt` e rever o diff. |
| `make test` | Executa `go test ./...`. | Todos os testes passam; investigar a primeira falha antes de alterar o teste. |
| `make vet` | Executa `go vet ./...`. | Sem diagnósticos; corrigir a causa indicada pelo Go. |
| `make build` | Compila `cmd/api` para `bin/payment-api`. | Ficheiro criado; se falhar, validar a versão Go e a mensagem de compilação. |
| `make ci` | Executa formatação, testes, vet e compilação. | Código de saída 0; é o conjunto de verificações Go anterior à revisão/CI. |
| `make docker-build` | Constrói a imagem local `novabanco-payment-api:local`. | Imagem criada; em caso de falha, verificar Docker/daemon e saída do build. |

Requisitos locais: Go 1.23 ou superior, como declarado em `go.mod`, `make` e Docker Engine/Desktop para os alvos Docker. O Dockerfile e a CI usam Go 1.26.x; esta fase ainda não verificou a aplicação com o toolchain Go 1.23. Confirma a versão com `go version`, `make --version` e `docker version`. Os testes unitários não precisam de AWS; a execução da API ou testes de integração com DynamoDB precisam de configuração e recursos próprios.

## CI: o que é verificado e o que fica fora

O workflow `.github/workflows/ci.yml` corre em pull requests, em pushes para `main` e manualmente. Os passos são sequenciais: checkout; instalação da toolchain Go; verificação `gofmt`; `go vet`; testes; compilação; build da imagem; scan Trivy. A imagem é construída com `push: false` e carregada apenas no runner para ser analisada. **Esta fase não publica imagens, não assume uma role AWS e não tem permissões OIDC**; essas responsabilidades pertencem à fase A3.

As Actions são fixadas a SHAs completos para evitar que uma tag móvel altere silenciosamente o código executado. Os comentários no workflow registam a versão de release associada ao SHA; confirma releases e compatibilidade na documentação oficial quando fizeres atualizações. O token do workflow tem apenas `contents: read`. O scan falha perante vulnerabilidades `HIGH` ou `CRITICAL`; não ignores uma descoberta sem análise, decisão documentada e prazo para remover a exceção.

| Problema | Contributo desta CI | Prova ainda necessária |
|---|---|---|
| Falha regional | Compila e testa a lógica idempotente incluída no código. | Game Day ativo-passivo, RTO/RPO medidos e reconciliação no repositório de infraestrutura. |
| Pedido repetido | Testes unitários verificam que o mesmo identificador não volta a debitar/creditar na região que já registou a operação. | Teste de retry durante failover e contagem de débitos duplicados com DynamoDB/ambiente de teste. |
| Atualização durante pagamentos | Constrói a imagem com o binário que implementa encerramento gracioso e verifica vulnerabilidades da imagem. | Rolling update sob carga, medindo respostas não-2xx e pedidos perdidos; configuração Kubernetes pertence ao repo de infraestrutura. |

### Escolha do serviço de CI e do registo

GitHub Actions foi escolhido por o código estar no GitHub, por integrar verificações de pull request sem manter um servidor de CI e por permitir OIDC na fase de publicação. Jenkins é uma alternativa válida quando a organização já opera essa plataforma ou precisa de controlo próprio, mas exige manter controlador, agentes, atualizações e gestão de credenciais. Nesta fase a CI não usa AWS.

ECR é a escolha prevista para produção por estar próximo dos workloads AWS e suportar a integração de identidade e a replicação planeada para DR. O destino ECR só será configurado na A3.

| Registo | Vantagem | Custo/limitação a confirmar antes da decisão |
|---|---|---|
| Amazon ECR (escolhido) | Integração AWS e replicação entre regiões, alinhadas com EKS e DR. | Requer configuração AWS, IAM e atenção a armazenamento, transferência e retenção. |
| GitHub Container Registry (GHCR) | Próximo do código e dos workflows GitHub; conveniente para projetos centrados em GitHub. | O acesso de workloads EKS e a estratégia de replicação precisam de ser desenhados. |
| Docker Hub | Ecossistema amplo e simples para imagens públicas. | Limites de pull, funcionalidades e preços dependem do plano e podem mudar. |

Confirma limites e preços nas páginas oficiais antes de ativar serviços pagos; não são estimados aqui. A imagem local construída nesta fase não é enviada para nenhum destes registos.

### Documentação oficial

- [Instalar Go](https://go.dev/doc/install)
- [Builds multi-stage do Docker](https://docs.docker.com/build/building/multi-stage/)
- [Cache de builds do Docker](https://docs.docker.com/build/cache/)
- [GitHub Actions](https://docs.github.com/actions)
- [Amazon ECR](https://docs.aws.amazon.com/AmazonECR/latest/userguide/what-is-ecr.html)
- [GitHub Container Registry](https://docs.github.com/packages/working-with-a-github-packages-registry/working-with-the-container-registry)
- [Docker Hub](https://docs.docker.com/docker-hub/)
- [Trivy Action](https://github.com/aquasecurity/trivy-action)

## Limitações e próximos passos

- Esta versão não autentica nem autoriza clientes. Não a exponhas à Internet nem a uses com contas reais.
- Só aceita USD e não implementa câmbio, criação de contas ou consulta do histórico.
- `/readyz` verifica acesso à tabela e se a região está autorizada a escrever; não prova por si só que todas as operações de negócio funcionarão. Usa `/livez` como liveness para não reiniciar contentores por uma falha temporária do DynamoDB.
- A proteção idempotente é regional durante o período de replicação. O failover ativo-passivo, a reconciliação, o RTO/RPO e o rolling update ainda precisam de provas nos Game Days.
- A aplicação e a infraestrutura vivem em repositórios distintos: este repositório contém código, contrato, testes e CI; Terraform, EKS, IAM, Global Tables, DNS e GitOps pertencem ao repositório de infraestrutura.
- A CI atual valida código e imagem; ainda não publica artefactos, executa testes com DynamoDB, mede carga nem prova failover ou rolling update.
