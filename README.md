# GRUALINK API: ejemplo en Go

[English](README.en.md)

Ejemplo listo para clonar y correr que integra tu sistema (aseguradora, asistencia, integrador) con la
API de GRUALINK: **pedir una grúa**, **consultar el estatus** y **recibir avisos firmados**.

Guía pública de la API: <https://grualink.mx/api>

Qué trae:

- Un **cliente mínimo** (`grualink/client.go`): crea solicitudes con `Idempotency-Key`, consulta el
  estatus, errores tipados, reintentos con espera exponencial y tiempo máximo por `context.Context`.
- Un **receptor de avisos** (`grualink/webhooks.go`, `grualink/webhook_handler.go`): un `http.Handler`
  que verifica la firma sobre el cuerpo crudo, valida la ventana de tiempo, soporta el cambio de secreto
  y descarta duplicados.
- Dos **ejemplos ejecutables** (`examples/`).
- El **kit de contrato** (`contract/`): el contrato de la API en español e inglés y los casos de prueba
  de firma que comparten los ejemplos de todos los lenguajes.

Solo usa la biblioteca estándar. No es un SDK: es código pensado para leerse y copiarse a tu proyecto.

## Requisitos

- Go 1.22 o superior.
- Una **llave de prueba** (`glk_test_…`). Te la da la empresa de grúas con la que trabajas.
- Para los avisos: el **secreto de firma** (`whsec_…`) que te entrega la misma empresa al registrar la
  dirección de tu sistema.

## Instalación

```bash
git clone <la dirección de este repositorio>
cd grualink-api-go
go build ./...
```

No hay dependencias que descargar.

## Variables de entorno

Solo los nombres; los valores van en tu equipo y nunca en el repositorio. Hay una plantilla en
`.env.example` (los ejemplos leen el entorno del proceso; no cargan archivos `.env`).

| Variable | Para qué |
|---|---|
| `GRUALINK_API_KEY` | Tu llave de **prueba** (`glk_test_…`) |
| `GRUALINK_WEBHOOK_SECRET` | Secreto de firma de los avisos (`whsec_…`), completo, con su prefijo |
| `GRUALINK_WEBHOOK_SECRET_PREVIOUS` | Opcional: el secreto anterior mientras dura un cambio de secreto |
| `GRUALINK_API_BASE_URL` | Opcional: otra dirección base (por omisión `https://grualink.mx/api/v1`) |
| `PORT` | Opcional: puerto del receptor de ejemplo (por omisión 8787) |

## Correr los ejemplos

### Pedir una grúa

```bash
export GRUALINK_API_KEY=...   # tu llave de prueba
go run ./examples/request-tow
```

Con datos ficticios, el ejemplo envía una solicitud, imprime el precio estimado, la vuelve a enviar con
la misma `Idempotency-Key` para mostrar que no se duplica y consulta su estatus. Solo corre con llaves
de prueba.

Con una llave de prueba, el inicio de `external_ref` decide el estatus final: `TEST-CONFIRMAR`,
`TEST-EN-CURSO`, `TEST-TERMINAR`, `TEST-RECHAZAR`, `TEST-CANCELAR`.

### Recibir avisos

```bash
export GRUALINK_WEBHOOK_SECRET=...   # tu secreto de firma
go run ./examples/webhook-server
```

Queda escuchando en `http://localhost:8787/webhooks/grualink`. GRUALINK solo entrega a direcciones
`https` públicas en el puerto 443: publica el receptor detrás de tu dominio (o de un túnel `https` mientras
pruebas), dale esa dirección a la empresa y pídele que te mande un aviso de prueba.

## Uso en tu código

```go
client, err := grualink.NewClient(os.Getenv("GRUALINK_API_KEY"))
if err != nil {
	return err
}

// El contexto limita la llamada completa, con reintentos y esperas; cada intento tiene además
// su propio tiempo máximo (15 s; se cambia con grualink.WithTimeout).
ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
defer cancel()

result, err := client.CreateServiceRequest(ctx, grualink.ServiceRequestInput{
	ExternalRef: "TEST-CONFIRMAR-0001",
	Contact:     grualink.Contact{Name: "Persona de Ejemplo", Phone: "+525500000000"},
	Pickup: grualink.Place{
		Address: "Av. Juárez 100, Centro, Guadalajara, Jal.",
		Lat:     grualink.Float64(20.6767),
		Lng:     grualink.Float64(-103.3475),
	},
	Destination: &grualink.Place{
		Address: "Av. Patria 500, Zapopan, Jal.",
		Lat:     grualink.Float64(20.7214),
		Lng:     grualink.Float64(-103.3918),
	},
}, grualink.CreateServiceRequestOptions{IdempotencyKey: miRegistro.ClaveDeIdempotencia}) // guárdala antes de llamar

var apiErr *grualink.APIError
switch {
case errors.Is(err, grualink.ErrValidation) && errors.As(err, &apiErr):
	log.Println(apiErr.Details)
case err != nil:
	return err
default:
	sr := result.ServiceRequest
	log.Println(sr.ID, sr.Status, result.Replayed)
}
```

Las demás operaciones: `client.GetServiceRequest(ctx, id)`,
`client.FindServiceRequestsByExternalRef(ctx, referencia)` y `client.Health(ctx)`.

### Idempotencia y reintentos

- Cada solicitud lleva una `Idempotency-Key` tuya. Si repites la llamada con la misma clave y el mismo
  cuerpo en 24 horas, la API responde **`200`** con `Idempotent-Replayed: true` y no crea nada
  (`Replayed` es `true`). La misma clave con otro cuerpo responde `409`.
- El cliente reintenta solo lo que tiene sentido: errores de red, tiempo agotado, `5xx` y
  `429 rate_limited` (esperando lo que diga `Retry-After`). Usa espera exponencial con jitter y reenvía
  siempre la misma clave y los mismos bytes: el cuerpo se serializa una sola vez.
- Si el contexto que pasas se cancela o vence, la llamada termina de inmediato y no se reintenta.
- No sigue redirecciones.
- La llave nunca aparece en errores ni registros, ni al imprimir el cliente.

### Verificar un aviso

```go
result := grualink.VerifyWebhookSignature(grualink.VerifyWebhookSignatureInput{
	RawBody:         rawBody,                                       // los bytes tal como llegaron
	SignatureHeader: r.Header.Get(grualink.SignatureHeader),
	Secrets:         []string{os.Getenv("GRUALINK_WEBHOOK_SECRET")}, // completo, con whsec_
})
// result.Valid, result.Reason
```

Lo importante:

1. Verifica el **cuerpo crudo**, antes de convertirlo a JSON. Si lo conviertes y lo vuelves a
   serializar, la firma ya no coincide.
2. Usa el secreto **completo**, con su prefijo `whsec_`.
3. Rechaza marcas de tiempo con más de 5 minutos de diferencia.
4. Tras un cambio de secreto llegan dos firmas `v1` durante 24 horas: acepta si cualquiera coincide.
5. Los avisos pueden repetirse: guarda el `id` de los ya procesados y responde `2xx` sin reprocesar.
6. Responde `2xx` en menos de 10 segundos y haz el trabajo pesado después.

### Montar el receptor

`grualink.NewWebhookHandler` devuelve un `http.Handler`. Con `net/http`:

```go
handler := grualink.NewWebhookHandler(grualink.WebhookHandlerOptions{
	Secrets: []string{os.Getenv("GRUALINK_WEBHOOK_SECRET")},
	OnEvent: func(ctx context.Context, event grualink.WebhookEvent) error {
		return guardarEnMiCola(ctx, event) // rápido; si devuelve un error se responde 500 y el aviso se reintenta
	},
})

mux := http.NewServeMux()
mux.Handle("/webhooks/grualink", handler)
```

El punto delicado es conseguir el **cuerpo crudo**: nada debe leer ni convertir el cuerpo antes del
receptor. Como es un `http.Handler`, se monta igual en los enrutadores habituales; por ejemplo, con chi
`r.Method(http.MethodPost, "/webhooks/grualink", handler)` y con Gin
`router.POST("/webhooks/grualink", gin.WrapH(handler))`, sin un middleware que lea el cuerpo en esa ruta.
Si tu framework no acepta un `http.Handler`, lee tú los bytes y llama a
`handler.HandleDelivery(ctx, rawBody, header)`, que devuelve el estatus y el cuerpo de la respuesta.

Con varias instancias, sustituye `InMemoryDeliveryDeduplicator` por una implementación de
`DeliveryDeduplicator` sobre tu base de datos (basta una clave única sobre el `id` del aviso).

## Errores

Todos los errores de la API tienen la forma `{ "error": { "code", "message", "details"? } }`. El `code`
es estable; decide con él, no con el `message`.

En Go los errores se distinguen con `errors.Is` contra los valores de la tabla, y los datos de la
respuesta (`Status`, `Code`, `Message`, `Details`, `RetryAfterSeconds`, `Attempts`) se leen con
`errors.As` sobre `*grualink.APIError`. Todo error del paquete cumple `errors.Is(err, grualink.ErrGrualink)`
y todo error respondido por la API cumple además `errors.Is(err, grualink.ErrAPI)`.

| HTTP | `code` | Qué pasó | `errors.Is` | ¿Se reintenta? |
|---|---|---|---|---|
| 400 | `invalid_request` | La petición no es válida | `ErrInvalidRequest` | no |
| 400 | `invalid_json` | El cuerpo no es JSON válido | `ErrInvalidRequest` | no |
| 400 | `key_in_query` | La llave se envió en la URL; pide que la revoquen | `ErrInvalidRequest` | no |
| 400 | `idempotency_key_required` | Falta `Idempotency-Key` | `ErrInvalidRequest` | no |
| 400 | `invalid_idempotency_key` | `Idempotency-Key` inválida (1 a 255 caracteres ASCII visibles) | `ErrInvalidRequest` | no |
| 401 | `unauthorized` | Llave ausente, inválida, vencida o revocada | `ErrAuthentication` | no |
| 403 | `insufficient_scope` | La llave no tiene permiso para esto | `ErrPermission` | no |
| 403 | `customer_unavailable` | Tu cuenta de cliente con la empresa no está disponible | `ErrPermission` | no |
| 403 | `live_mode_unavailable` | Las solicitudes reales aún no están habilitadas | `ErrPermission` | no |
| 403 | `browser_not_allowed` | Llave real usada desde un navegador | `ErrPermission` | no |
| 404 | `not_found` | No existe o no es tuya | `ErrNotFound` | no |
| 405 | `method_not_allowed` | Método no permitido para esa ruta | `ErrInvalidRequest` | no |
| 409 | `idempotency_conflict` | Esa `Idempotency-Key` ya se usó con otro cuerpo | `ErrIdempotencyConflict` | no |
| 413 | `payload_too_large` | El cuerpo excede 16 KB | `ErrInvalidRequest` | no |
| 415 | `unsupported_media_type` | El cuerpo debe ir como `application/json` | `ErrInvalidRequest` | no |
| 422 | `validation_error` | Campos inválidos; `Details` dice cuáles | `ErrValidation` | no |
| 429 | `rate_limited` | Demasiadas peticiones; espera `Retry-After` | `ErrRateLimit` | sí |
| 429 | `too_many_open_requests` | Demasiadas solicitudes sin resolver con la empresa | `ErrRateLimit` | no |
| 500 | `internal_error` | Error interno | `ErrServer` | sí |

Además, con el tipo `*grualink.Error`: `ErrConnection` (sin respuesta, se reintenta), `ErrTimeout`
(tiempo agotado, se reintenta) y `ErrConfig` (configuración o argumento inválido; no se envió nada).
Cuando termina el contexto que pasaste, el error cumple también `errors.Is(err, context.Canceled)` o
`errors.Is(err, context.DeadlineExceeded)`.

Una respuesta de error cuyo cuerpo no tiene la forma documentada lleva el código `unexpected_response`.

El contrato completo, con campos, límites y el orden de las comprobaciones, está en
[`contract/API.es.md`](contract/API.es.md).

### Nombres del contrato en Go

El contrato nombra las operaciones igual en todos los lenguajes. En Go se usan sin repetir el nombre
del paquete:

| Contrato | Go |
|---|---|
| `GrualinkClient` | `grualink.Client` (`grualink.NewClient`) |
| `createServiceRequest`, `getServiceRequest`, `findServiceRequestsByExternalRef`, `health` | `Client.CreateServiceRequest`, `Client.GetServiceRequest`, `Client.FindServiceRequestsByExternalRef`, `Client.Health` |
| `verifyWebhookSignature`, `computeWebhookSignature`, `buildSignatureHeader` | `grualink.VerifyWebhookSignature`, `grualink.ComputeWebhookSignature`, `grualink.BuildSignatureHeader` |
| `DeliveryDeduplicator`, `InMemoryDeliveryDeduplicator` | `grualink.DeliveryDeduplicator` (`Claim`, `Release`), `grualink.InMemoryDeliveryDeduplicator` |
| `createWebhookHandler` | `grualink.NewWebhookHandler` |
| `GrualinkError` | `grualink.ErrGrualink` |
| `GrualinkConfigError`, `GrualinkConnectionError`, `GrualinkTimeoutError` | `grualink.ErrConfig`, `grualink.ErrConnection`, `grualink.ErrTimeout` (tipo `*grualink.Error`) |
| `GrualinkApiError` | `grualink.ErrAPI` (tipo `*grualink.APIError`) |
| `GrualinkInvalidRequestError`, `GrualinkAuthenticationError`, `GrualinkPermissionError`, `GrualinkNotFoundError` | `grualink.ErrInvalidRequest`, `grualink.ErrAuthentication`, `grualink.ErrPermission`, `grualink.ErrNotFound` |
| `GrualinkIdempotencyConflictError`, `GrualinkValidationError`, `GrualinkRateLimitError`, `GrualinkServerError` | `grualink.ErrIdempotencyConflict`, `grualink.ErrValidation`, `grualink.ErrRateLimit`, `grualink.ErrServer` |

## Pruebas

```bash
go test -race ./...   # pruebas del paquete y de los dos ejemplos
go vet ./...
gofmt -l .            # no debe listar nada
```

Las pruebas no llaman a la API real: usan servidores falsos locales (`net/http/httptest`). Cubren todos
los casos de `contract/signature-vectors.json`, cada código de error, los reintentos, la idempotencia y
que la llave no aparezca en errores ni registros. `examples/examples_test.go` compila y ejecuta los dos
ejemplos contra un servidor falso.

## Estructura

```
contract/     contrato de la API (es/en), casos de prueba de firma y lista de verificación
grualink/     cliente, errores, tipos, verificación de avisos y receptor, con sus pruebas
examples/     request-tow y webhook-server, y la prueba que los ejecuta
```

La carpeta `contract/` es una copia sin cambios de la que comparten los ejemplos de todos los lenguajes;
no se edita aquí.

## Licencia

[MIT](LICENSE)
