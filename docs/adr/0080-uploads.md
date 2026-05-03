# ADR 0080 — Uploads de mídia (whitelist + re-encode + cap)

- Status: Accepted
- Data: 2026-05-02
- Origem: Plano [SIN-62226](../../) §5–§7 · F47 (HIGH) + F48 (MEDIUM)
- Issue: SIN-62246
- Substitui: —
- Substituído por: —

## Contexto

O CRM aceita uploads em dois lugares:

1. Logo de tenant (admin-only, pequeno).
2. Anexo de mensagem do canal (cliente final, mais permissivo).

Sem controles, navegadores e o pipeline de processamento de imagem expõem o
sistema a quatro classes de ataque conhecidas:

- **SVG-XSS** — `<script>` ou `<foreignObject>` dentro de SVG é executado quando
  o documento é servido inline; mesmo em `<img src=…>` o vetor existe via
  attributos `onload`/`onerror`. Não há sandbox barata.
- **Polyglot files** — um arquivo que é PNG válido E HTML/JS válido ao mesmo
  tempo (e.g. `.png` com `<script>` em um chunk `tEXt`, ou um GIF/JS hybrid).
  `Content-Type` correto não impede execução se servido em origin com cookie.
- **Decompression bombs** — header de PNG declara 100 000 × 100 000 px; o
  decoder aloca 40 GB de RAM antes de qualquer validação posterior.
- **Content-Type spoofing** — cliente envia `Content-Type: image/png` mas
  payload começa com `<?xml` ou `<!DOCTYPE`. Confiar no header dado pelo
  cliente é OWASP A03.

Frameworks como ImageMagick mitigam alguns desses ataques mas trazem CVEs
próprios (Ghostscript, MVG, vulnerable coders). A escolha de defesa em
profundidade aqui é deliberadamente aborrecida: stdlib do Go, mais
`golang.org/x/image/webp`.

## Decisão

### §5 Whitelist + re-encode obrigatório

**Tipos aceitos por endpoint** (ports + policy do módulo
`internal/media/upload`):

| Endpoint                   | Formatos                  | Max bytes | Max pixels         |
|----------------------------|---------------------------|-----------|--------------------|
| Logo de tenant             | PNG, JPEG, WEBP           | 2 MB      | 1024 × 1024        |
| Anexo de mensagem          | PNG, JPEG, WEBP, PDF      | 20 MB     | (cap geral 16 Mpx) |

- **SVG é rejeitado** com `415 unsupported_media_type` em todos os endpoints.
  Render-side sanitization (DOMPurify-equivalent server-side) foi descartado:
  custo alto, superfície grande, e nenhum caso de uso real do produto
  precisa de SVG hoje. ADR follow-up `0080a-svg-policy.md` re-considerará v2
  com requisitos do front.
- **Magic-byte check obrigatório** antes de decodar:
  - PNG: `89 50 4E 47 0D 0A 1A 0A`
  - JPEG: `FF D8 FF`
  - WEBP: bytes 0–3 `52 49 46 46` (`RIFF`) e bytes 8–11 `57 45 42 50` (`WEBP`)
  - PDF: `%PDF-`
  - Mismatch entre `Content-Type` declarado e magic-byte → `415`.
  - Mismatch entre `Content-Type` e extensão também → `415`.
  - **Nunca confiar** no `Content-Type` enviado pelo cliente.
- **Decompression-bomb cap**: depois do decode-header (que não aloca o
  framebuffer), `width × height ≤ 16 777 216` (16 Mpx). Acima disso →
  `ErrDecompressionBomb` antes de qualquer alocação.
- **Re-encode obrigatório** para todo formato de imagem: decode usando
  `image/png`, `image/jpeg` ou `golang.org/x/image/webp`; re-encode usando o
  encoder canônico do mesmo formato. Isso strips:
  - EXIF (JPEG `APP1`, PNG `eXIf`)
  - Chunks PNG ancillary não-padrão (`tEXt`, `iTXt`, `zTXt`, `sPLT`, etc.)
  - ICC profiles desconhecidos (mantemos só sRGB implícito)
  - Trailer bytes após `IEND` (PNG) ou após o `EOI` (JPEG)
  - Dados arbitrários em `RIFF` chunks WEBP fora do esperado
- **PDF é exceção ao re-encode**: re-encodar PDF não é viável com stdlib e
  abriria mais superfície do que fecha. PDFs **bypass o re-encoder mas
  passam por scanner antimalware** ([SIN-62228](#)). Esse handoff está
  fora deste bundle.
- **Hash + dedupe**: `SHA-256` é tirado do binário **re-encodado**, não do
  raw input. Isso garante:
  - Deduplicação real por tenant (mesmo logo enviado 2× resulta em mesma
    row em `media`).
  - O hash representa o que de fato vai ao storage.
- **Path no storage**: `media/<tenant_id>/<yyyy-mm>/<hash>.<ext>`. O
  caminho é montado no servidor e **nunca derivado de input do usuário**
  (filename, content-disposition, etc.). Isso elimina path traversal
  (`../etc/passwd`), null-byte tricks, e CRLF injection.

### §6 Headers de resposta para mídia servida

Quando a mídia é servida para download:

- `Content-Type` exato derivado do formato re-encodado (jamais reflexivo do
  upload).
- `Content-Disposition: attachment; filename="<sanitized>"` em endpoints onde
  o uso é "baixar" — força download em vez de render inline.
- `X-Content-Type-Options: nosniff` em **todas** as respostas. Sem isso,
  Internet Explorer, Edge legacy e antigos Chromes deduzem `text/html` a
  partir de bytes "que parecem HTML" mesmo quando declaramos imagem.
- `Cache-Control: private, max-age=86400, immutable` para conteúdo já
  hash-addressed.
- `Cross-Origin-Resource-Policy: same-origin` para impedir hot-link e
  side-channel via `<img>` em outras origens (defesa contra Spectre-class
  leaks).
- `Content-Security-Policy: default-src 'none'; sandbox` na resposta da
  mídia mesmo que ela seja imagem — barreira em profundidade caso um
  polyglot escape o re-encode.

### §7 Cookieless static origin

Impl em outra issue (E). Decisão: as imagens vão num subdomínio sem cookies
de sessão (`media.crm.example.com`), com seu próprio Caddy upstream. Isso
elimina:

- Stored-XSS via `<script>` em SVG (já rejeitado, mas defesa em profundidade).
- CSRF por mídia que escape o re-encode.
- Token leak em `Referer` quando a mídia é servida.

A separação cookieless está fora deste bundle e será amarrada via ADR
adendo no merge da issue E.

## Consequências

**Positivas:**

- Quatro classes de ataque (SVG-XSS, polyglot, bomb, type-spoof) cobertas
  em uma única camada com 4 controles independentes (defense-in-depth real,
  não 4 nomes para a mesma coisa).
- Nenhuma dep nova além de `golang.org/x/image` (sub-projeto oficial Go).
- Re-encode é determinístico: hash final é estável → dedupe funciona.
- Path no storage é completamente livre de input do usuário → impossível
  path traversal.

**Negativas / custos:**

- Re-encode tem custo de CPU. Para uma imagem de 2 MB JPEG ~30 ms p50 num
  core moderno; aceitável dado que uploads não são hot path.
- Re-encode pode ter perda perceptual em JPEG (re-quantização). Mitigado
  com `Quality=90` no encoder, que é o sweet-spot empírico.
- WEBP em stdlib é **decode-only** (`x/image/webp` não tem encoder). Saída
  WEBP é re-encodada como **PNG** (lossless, sem deps adicionais). Isso é
  documentado no `policy.OutputFormat` e nos testes.
- `golang.org/x/image` adiciona ~100 KB ao binário.

**Trade-offs explicitamente recusados:**

- ImageMagick / `libvips`: superfície de CVE alta demais para o ganho.
- Background re-encode async: complica retry/idempotency. Re-encode síncrono
  no upload-handler é simples e o p99 cabe no SLA.
- Permitir SVG via DOMPurify server-side: superfície grande, sem caso de uso
  hoje. Re-considerar via `0080a` se o produto demandar.

## Implementação

- Módulo puro: `internal/media/upload`
  - `Decoder` port: `bytes → (image.Image, format, error)`
  - `ReEncoder` port: `(image.Image, format) → (bytes, error)`
  - `Process(ctx, raw, policy) (Result, error)` — orquestra magic-byte +
    pixel cap + decode + re-encode + hash.
- Adapter: `adapters/imagecodec/stdlib` — implementa `Decoder` e `ReEncoder`
  via stdlib + `x/image/webp`.
- Persistência (`media.content_hash`, paths, storage backend) é amarrada
  pelo handler, fora deste bundle. O módulo retorna apenas o `Result`.

## Referências

- F47 (HIGH): SVG e polyglot uploads — plano [SIN-62226 §5](#).
- F48 (MEDIUM): decompression-bomb cap — plano [SIN-62226 §5](#).
- OWASP ASVS V12.3 (File handling).
- OWASP A03 (Injection / type confusion).
- ADR follow-up: `0080a-svg-policy.md` (fora deste bundle).
