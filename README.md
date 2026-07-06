# quick

Hosting interno in stile [Quick di Shopify](https://shopify.engineering/quick):
pubblichi una cartella di HTML/asset e ottieni `<nome>.<tuo-dominio>`. Di default
un sito è visibile solo agli account del dominio aziendale (SSO Google), ma puoi
aprirlo al pubblico o proteggerlo con un codice, e bloccarne la sovrascrittura.

Tutto è configurabile da variabili d'ambiente: nessun dominio o credenziale è
cablato nel codice. Lo storage può essere locale o object storage S3-compatibile.

## Installare la CLI

Binario già pronto, nessun Go richiesto (lo scarica dall'ultima release e lo
serve il tuo stesso dominio):

```bash
# macOS / Linux
curl -fsSL https://<il-tuo-dominio>/install.sh | sh
# Windows (PowerShell)
irm https://<il-tuo-dominio>/install.ps1 | iex
```

Oppure, se hai Go:

```bash
go install github.com/zupolgec/quick/cmd/quick@latest   # finisce in $(go env GOBIN)
```

Per aggiornare: `quick upgrade` (scarica e rimpiazza il binario con l'ultima
release; `quick upgrade --check` controlla soltanto). Chi ha installato con Go usa
di nuovo `go install …@latest`.

## Pubblicare un sito

```bash
export QUICK_SERVER=https://quick.example.com   # una volta (o usa --server)
quick login                                     # login Google nel browser
quick deploy foo ./ilmiosito                    # -> https://foo.quick.example.com
```

La sintassi è `quick deploy <sito> [cartella]` (cartella opzionale, default quella
corrente). Senza `<sito>` usa il `.quick` della cartella o, in mancanza, il nome
della cartella stessa.

Il deploy è un **mirror**: carica la cartella e sostituisce l'intero sito (i file
rimossi spariscono). Prima di pubblicare la CLI mostra un riepilogo e **chiede
conferma** (`--yes` per saltarla, `--dry-run` per vedere cosa salirebbe senza
pubblicare); un deploy senza file è **bloccato** (`--force` per svuotare di
proposito). Sottodomini nuovi sono istantanei (il wildcard copre già il certificato).

**Cosa non viene pubblicato**, in tre livelli: (1) sicurezza, sempre — file nascosti
(`.git`, `.env`, `.quick`… tranne `.well-known`) e segreti (`*.pem`, `*.key`, `id_rsa`,
keystore); (2) comodità, predefiniti e scavalcabili — `node_modules/`, `vendor/`,
`*.log`, temporanei; (3) `.quickignore`, se presente, è la fonte di verità delle
esclusioni di comodità (sintassi gitignore, con `!`). Crealo con `quick ignore`.

**Convenzioni del sito servito**: `index.html` come indice di cartella; `/about`
serve `about.html`, mentre `about/index.html` viene servito da `/about/`. Gli HTML
hanno URL canonici: `/about.html` → `/about`, `/about/index.html` → `/about` e, se
è una cartella, `/about` → `/about/`. `404.html` può stare nella radice o nella
cartella più vicina al path mancante (status 404); `200.html` in radice è l'app
shell SPA (servita 200 per le rotte che non sono file). Senza `200.html`, un path
mancante dà un 404 vero.

`quick status` mostra server, login, visibilità del sito e cosa salirebbe col deploy.
`quick skill` pubblica una Agent Skill (`SKILL.md`) che insegna a un agente come usare
la CLI. `SKILL.md` è un formato aperto cross-vendor (Claude Code, Codex, Gemini,
Cursor…): default `~/.claude/skills/quick/`, oppure `--target codex|gemini|…`,
`--project` (cartella del repo), `--all` (tutti gli agenti noti).

Al primo deploy viene scritto un file **`.quick`** nella **cartella corrente** (nome
del sito, server e la sottocartella pubblicata), così da lì puoi ripetere senza
parametri: `quick deploy`, `quick publish`, ecc. Esempio: `quick deploy foo ./build`
scrive `.quick` con `dir: build`, quindi un `quick deploy` nudo dalla stessa cartella
ripubblica `./build` su `foo` — anche se `build` viene rigenerata ogni volta.
(`.quick` non viene caricato.) Se provi a fare deploy su un sito diverso da quello
collegato, la CLI te lo segnala e chiede conferma.

Il server si dà come dominio nudo (`quick.example.com`) o URL completo; la CLI
aggiunge `https://`, prova anche `deploy.<dominio>`, e ricorda quello che risponde.

## Visibilità e lock

```bash
quick publish   foo            # aperto a chiunque, niente SSO
quick unpublish foo            # torna dietro l'SSO aziendale (default)
quick private   foo            # accesso con codice (lo genera e te lo stampa)
quick private   foo --code abc # accesso con codice scelto da te
quick lock      foo            # da ora solo tu puoi sovrascrivere foo
quick unlock    foo
```

Il cambio di visibilità è **istantaneo** (solo un file di metadata). La decisione
di accesso la prende `quick-server`: pubblico → servito sempre; codice → pagina di
inserimento codice, poi un cookie firmato vale 7 giorni; SSO → verifica la sessione
Google via oauth2-proxy. Il **lock** registra te (dalla tua identità Google) come
owner: gli altri non possono più sovrascrivere né cambiare policy finché non fai
`unlock`.

Al deploy la CLI mostra **chi sei** (`Autenticato come …`) e, se l'ultima
pubblicazione di quel sito è di un'altra persona, chiede di **ridigitare il nome
del sito** prima di sovrascriverne il lavoro.

## Annullare un deploy

```bash
quick rollback foo   # ripristina la versione precedente (un secondo rollback la rifà)
```

Ogni deploy conserva la versione precedente: `rollback` le scambia. Disponibile
sullo storage locale; sull'object storage va gestito col versioning del bucket.

## Chi può modificare un sito

Di chi è un sito viene tracciato al primo deploy (creatore) e a ogni
aggiornamento. La regola di chi può sovrascrivere/eliminare/cambiare visibilità
è impostabile sul server con `QUICK_OWNERSHIP`:

- `free` (default): chiunque, in azienda, può tutto.
- `shared`: chiunque pubblica i contenuti, ma solo il creatore elimina o cambia
  la visibilità.
- `owned`: solo il creatore può intervenire sul sito.

In più, il creatore può `quick lock` il suo sito per riservarne a sé la modifica
in qualunque modalità (`quick unlock` per riaprirlo).

## Eliminare un sito

```bash
quick delete foo     # rimuove contenuti e metadata (irreversibile)
```

L'eliminazione chiede conferma; se il sito è pubblico o protetto da codice devi
ridigitarne il nome.

## Roadmap (convenzioni static-host, next step)

Convenzioni "via file, zero config" non ancora implementate, in ordine di utilità:

- **`_redirects`** (stile Netlify): regole di redirect/rewrite per sito, incluso il
  catch-all SPA esplicito (`/* /index.html 200`) come alternativa a `200.html`.
- **`_headers`**: header per-path (Cache-Control, CSP…) definiti dal sito.
- **Default di `Cache-Control`**: HTML `no-cache`, asset con `max-age` lungo (al meglio
  con asset con hash nel nome).
- **Precompressi brotli/gzip**: servire `file.css.br`/`.gz` se presente e il browser
  lo accetta.

Lato piattaforma e integrazione agenti:

- **Endpoint `/mcp`**: esporre quick come server MCP (deploy, status, publish… come
  strumenti chiamabili da qualunque agente, non solo come documentazione). In Go ci
  sono SDK MCP ufficiali, quindi è la via naturale per le *azioni* cross-agent.

## Architettura

L'**apex** (`<BASE_DOMAIN>`) è il control plane: API, autenticazione e una
dashboard dei siti (dietro SSO). Ogni **sottodominio** è solo un sito.

```
browser ──https──> coolify-proxy (caddy-docker-proxy, TLS apex + wildcard via DNS-01)
                     │  label su quick-server:  <BASE_DOMAIN>, *.<BASE_DOMAIN> -> quick-server:8080
                     ▼
                 quick-server (smista per HOST):
                   <BASE_DOMAIN> (apex):  /api/health|config|deploy|sites|site/<n>/{policy,rollback}
                                          /oauth2/* -> oauth2-proxy (SSO Google, sign_in + callback)
                                          /         -> dashboard (loggato) | pagina accesso (guest)
                   <sub>.<BASE_DOMAIN>:   policy per-sito (public/code/sso) + serve dallo Storage
                                          /__quick/code -> pagina codice; SSO -> pagina di accesso
                 Storage: local (bind mount) | S3-compatibile (stateless)

CLI quick ── login Google PKCE (loopback) ──> ID token ──> POST <apex>/api/deploy | /api/site/.../…
```

Il proxy fa solo `reverse_proxy` verso quick-server: niente `file_server` né file
in `/dynamic`. Il routing vive nelle **label** del container (reload graceful, nessun
restart del proxy). Per il modello "backend" alla Quick (API condivise `quick.db`/
`quick.storage`/… chiamate dal frontend) il seam è pronto — namespace riservato
same-origin, identità già risolta dall'SSO, storage astratto — ma non è implementato.

## Componenti

| Path | Cosa |
|---|---|
| `cmd/quick/` | CLI: `login` (PKCE), `deploy`, `publish`/`private`/`lock`; si auto-configura da `/api/config` |
| `cmd/quick-server/` | Front unico: serve i siti, policy/gate, deploy, `/oauth2/*`, `/api/config` |
| `internal/quick/` | Contratto condiviso CLI↔server (DTO, validazione nomi, modi di accesso) |
| `internal/storage/` | Backend storage: `local` (FS) e `s3` (minio-go) |
| `docker-compose.yaml` | Stack per Coolify (label Caddy + env) |
| oauth2-proxy (env `OAUTH2_PROXY_*`) | SSO Google |

## Configurazione (env)

Vedi `.env.example`. In sintesi: `QUICK_BASE_DOMAIN`, `QUICK_ALLOWED_DOMAINS` (uno, lista `a,b`, o `*`),
`GOOGLE_CLIENT_ID/SECRET` (client OAuth **Web** per oauth2-proxy), `COOKIE_SECRET`,
`QUICK_META_SECRET`, `QUICK_OWNERSHIP`=`free|shared|owned`, `QUICK_STORAGE`=`local|s3` (+ `QUICK_S3_*`).

Fail-closed: in produzione `QUICK_META_SECRET`, `QUICK_ALLOWED_DOMAINS` e `QUICK_OAUTH_CLIENT_ID`
sono obbligatorie. Se ne manca una il server non parte (niente default insicuro, niente
account ammessi a sorpresa). Per ammettere qualsiasi account Google usa `*` esplicito. Solo
in sviluppo locale `QUICK_DEV_NOAUTH=1` salta questi controlli.

Client OAuth della CLI (`QUICK_CLI_CLIENT_ID` / `QUICK_CLI_CLIENT_SECRET`): due modi
- **Desktop app** → imposta solo l'ID; la CLI usa PKCE senza secret.
- **riuso di un client Web** (anche lo stesso di oauth2-proxy) → imposta ID + secret;
  il secret viene servito alla CLI via `/api/config` (accettabile per il client loopback
  PKCE-bound). È il modo per riusare un client Web esistente senza cablare nulla.

## Deploy su Coolify (4.1.x)

1. Crea una risorsa **Docker Compose** dal repo git (Coolify builda `quick-server`),
   oppure usa l'immagine già pubblicata `image: ghcr.io/zupolgec/quick-server:latest`
   (versionata a ogni release, multi-arch) invece di `build: .`.
2. Imposta env/secrets (vedi sopra) e, se `QUICK_STORAGE=local`, i due bind mount.
3. **Connect to Predefined Network → coolify** (così il proxy raggiunge il container).
4. `CF_API_TOKEN` deve essere nell'env del proxy (lo usa la label `caddy.tls.dns`).

Il routing è tutto nelle label: cambiare contenuto o policy non richiede toccare il
proxy. Il vecchio `quick.caddy` in `/dynamic` non serve più (va rimosso al cutover).

La label `caddy` copre **apex + wildcard** nello stesso blocco (`<BASE_DOMAIN>,
*.<BASE_DOMAIN>`), così l'apex serve il control plane. L'auth è sull'apex: nel
client OAuth **Web** di Google il redirect URI è `https://<BASE_DOMAIN>/oauth2/callback`
(non più un sottodominio `auth.`).

## Sviluppo locale

```bash
QUICK_DEV_NOAUTH=1 QUICK_BASE_DOMAIN=quick.localhost \
  QUICK_SITES_DIR=./sites QUICK_META_DIR=./meta QUICK_META_SECRET=dev \
  go run ./cmd/quick-server
```
`QUICK_DEV_NOAUTH=1` salta la verifica del token (solo locale). Per lo storage S3 si
testa con un MinIO in Docker (vedi `QUICK_S3_*`).

## Licenza

MIT — vedi [LICENSE](LICENSE).
