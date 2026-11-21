# rumbo-claude

Proveedor **externo** para usar una suscripción Claude Pro/Max desde rumbo.
Es un proceso independiente que habla Agent Provider Protocol v1 por stdin/stdout.
No modifica rumbo, no se registra automáticamente y no importa sus paquetes
`internal`. El código del protocolo está incluido para poder compilarlo sin
el repositorio de rumbo.

## Compilar y registrar

Requiere Go 1.27.3 o posterior, en Linux (usa `flock`).

```sh
# From the rumbo-claude directory:
go build -buildvcs=false -o rumbo-claude .
rumbo provider add claude "$PWD/rumbo-claude"
```

Después:

```sh
rumbo -p claude
```

En la interfaz de rumbo:

1. `/login`: abre en tu navegador el enlace que muestra el proveedor.
2. Inicia sesión con tu cuenta de Claude y acepta la autorización.
3. Pega el código `code#state` que muestra Anthropic (también acepta la URL
   final con `code` y `state`). La respuesta se solicita como secreta.
4. `/use claude` lista los modelos disponibles para tu cuenta.
5. `/account` muestra si hay una sesión guardada y el enlace al uso del plan.
6. `/logout` elimina los tokens locales de este proveedor. **No** revoca los
   tokens en Anthropic ni cierra sesiones de otros programas.

Se implementa el flujo **copy code / headless** de `../pi`, por lo que funciona
con el navegador en otra máquina, sin listener local ni apertura automática
del navegador. No se implementa la alternativa de callback HTTP de pi.

## Credenciales y opciones

- Tokens en `$XDG_CONFIG_HOME/rumbo-claude/claude.json`, o en
  `~/.config/rumbo-claude/claude.json` si no se define esa variable.
- Directorio privado (`0700`) y archivo privado (`0600`), escritura atómica.
- Renovación automática cinco minutos antes de caducar y un único reintento
  con renovación si la API rechaza un token con HTTP 401. Un bloqueo de archivo
  cancelable coordina los procesos y reutiliza la renovación que otro ya hizo.
- Los fallos temporales de red, sobrecarga y límites del endpoint OAuth se
  comunican a rumbo como reintentables. Una cancelación no dispara reintentos.
- Si OAuth devuelve los scopes concedidos, se comprueba `user:inference` antes
  de guardar los tokens; una renovación que omite scopes conserva los anteriores.
- No lee, importa ni modifica las credenciales de pi o Claude Code.
- No usa `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, perfiles de `ant` ni una
  URL de API del entorno como alternativa al login.
- `effort` accepts `low`, `medium`, `high`, `xhigh`, or `max`, depending on
  the model. With Rumbo versions after 0.5.0, use the following commands
  to set, inspect, or clear the provider preference.

  ```sh
  rumbo provider set claude effort=high
  rumbo provider show claude
  rumbo provider set claude effort=
  ```

  In the TUI, `/session-set effort=low` overrides the provider preference
  for that session. Use `/session-set effort=` to inherit it again. When
  neither level specifies an effort, the model uses its own default.

  After signing in, `rumbo provider models claude` lists model IDs. Use
  `rumbo provider set claude model=<model-id>` with an ID from the list
  to choose the starting model for new sessions.

  Rumbo's new configuration store does not import registrations from 0.5.0
  or earlier. When upgrading from those versions, register the executable
  again and reapply your provider settings.

Transmite texto en streaming, llamadas y resultados de herramientas, bloques de
razonamiento firmados, uso de tokens y tamaño de contexto cuando la API lo informa.
Los errores de límite temporal y sobrecarga se marcan para reintento, también
cuando llegan dentro del stream tras un HTTP 200. Un stream sin `message_stop`
y motivo de parada no se acepta como una respuesta completa; siempre se cierra
el cuerpo HTTP. Un contexto excesivo se comunica a rumbo para que pueda compactarlo.

Las herramientas conservan su JSON Schema completo, incluidos `$defs`, uniones
y `additionalProperties`. La consulta opcional del tamaño de contexto tiene un
máximo de cinco segundos y solo guarda valores válidos en caché: un fallo temporal
no se convierte en un tamaño desconocido permanente.

## Compatibilidad y advertencia

Esta integración replica los parámetros públicos de OAuth y la compatibilidad
Claude Code que utiliza pi: cliente OAuth, scopes, cabeceras `claude-cli`/betas,
prefijo de sistema de Claude Code y nombres canónicos de herramientas en la API.
Devuelve a rumbo los nombres originales de sus herramientas. **Añade ese prefijo
al sistema recibido**: es una decisión de este proveedor externo, no un cambio
al transcript ni a las reglas del repositorio de rumbo.

No es una integración oficial de Anthropic. El acceso de terceros mediante una
suscripción puede estar restringido por sus condiciones; implementarlo no implica
que esté autorizado. Anthropic puede rechazarlo, cambiar el flujo o restringir la
cuenta. Las peticiones aceptadas consumen los límites de la suscripción. No hay
fallback automático a facturación por API key.

Se ha comprobado contra servidores HTTP locales y mediante el protocolo real en
un subproceso. **No se ha iniciado sesión con una cuenta real ni se ha verificado
inferencia contra Anthropic**, para no usar credenciales ni consumir cuota.

## Pruebas

```sh
gofmt -l .
go vet ./...
go test -race ./...
go build -buildvcs=false -o rumbo-claude .
```

Las pruebas no llaman servicios externos. Cubren PKCE, estado OAuth, cancelación,
renovación y rotación compartidas, permisos, errores sin exposición de tokens,
streaming, herramientas, razonamiento, caché, opciones y ciclo JSON-RPC real.

## Procedencia

La base del adaptador de Messages, el almacenamiento y el servidor/tipos de
protocolo procede de rumbo (MIT, `LICENSE`), con modificaciones para funcionar
independientemente. El flujo OAuth y la compatibilidad Claude Code siguen
`pi/packages/ai/src/auth/oauth/anthropic.ts` y
`pi/packages/ai/src/api/anthropic-messages.ts` (MIT, `LICENSE.pi`).
