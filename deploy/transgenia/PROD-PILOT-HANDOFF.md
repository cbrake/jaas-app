# Preparación de la promoción del piloto

Fecha: 2026-10-26.

## Alcance

Esta entrega prepara un PR hacia `transgenia/production`. La integración y la
activación quedan para el operador. La calidad de transcripción y minutas se
evaluará durante el uso del piloto; este PR no acredita una mejora de calidad.

Incluye sin alterar el commit de #2:
`45ccaff4eaa3b22aa0a1ce89ab1f96249f7881ee`. Separa la captura browser del permiso
STT de JaaS y elimina la carga duplicada del script de visualizaciones.
PR #1 ya está integrado en la base de esta promoción.

## Continuación por SSH

El operador recibirá el prompt completo fuera del PR. En la sesión SSH:

1. Verificar repositorio, remoto, rama, SHA, cambios locales y estado del PR.
2. Leer [README.md](README.md) y [PLAN-1.1.md](PLAN-1.1.md) en el SHA candidato.
3. Revisar el candidato en un checkout separado y preservar el trabajo ajeno.
4. Confirmar CI y preparar los ajustes necesarios en una rama revisable.
5. Entregar los comandos manuales adaptados al entorno verificado.
6. Distinguir preparación, integración, activación y verificación real.

La CI ejecuta Node, build, Go tests, formato y vet. El target Docker test no
ejecuta Node; incluir `node --test tests/capture-options.test.cjs` si se valida
mediante ese target.

## Actualización de fuentes después del merge

La rama de esta promoción es `transgenia/production`, no `main`. Sólo desde el
checkout limpio, en esa rama, con `origin` verificado como este repositorio y
después de confirmar la integración del PR:

```sh
git pull --ff-only origin transgenia/production
```

Si el remoto correcto usa otro nombre, adaptar el comando. El pull actualiza las
fuentes; el operador debe seguir el runbook aplicable para activar el servicio.

## Evaluación y trazabilidad

Registrar privadamente la versión y los resultados observados. No incluir
contenido de reuniones en issues, PR o artefactos públicos. Diarización e
integración Odoo quedan fuera de esta promoción.

WebMCP: ninguna tool pública creada, modificada o retirada.
Registro: `audit/2026-10-26/meet-prod-pilot/fix_log.md`.
