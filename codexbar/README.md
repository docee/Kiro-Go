# Kiro Go Usage CodexBar plugin

`kiro-go-usage.js` is a local CodexBar provider for the Kiro Go personal usage
endpoint. It requests `GET /v1/usage?days=30`, keeps the API key in CodexBar's
secure settings, and reports credit and token quota windows plus daily charts.
Credit values are Kiro's internal metering units, not currency. The plugin marks
the snapshot as `dataConfidence: "estimated"` because upstream token reporting
can be estimated for some requests.

## Install

1. Copy `kiro-go-usage.js` into `~/.config/codexbar/providers/`, or use
   **Settings -> Plugins -> Install...** in CodexBar.
2. Review and approve the displayed endpoint and `bearer` authentication.
3. Set `BASE_URL` to the Kiro Go origin, such as `https://kiro.example.com`.
   Do not include `/v1/usage`; the plugin adds that path.
4. Set `API_KEY` to the personal key configured in Kiro Go, then enable the
   provider.

For a local HTTP server, use a loopback, RFC 1918, link-local, unique-local,
or `.local` address. CodexBar requires typed approval of every normalized
private-network origin. Public origins must use HTTPS; do not approve a public
HTTP endpoint.

## CLI verification

List the installed provider and inspect its manifest:

```sh
codexbar plugins list
codexbar plugins fetch kiro-go-usage --json --pretty
```

For headless CLI overrides, CodexBar maps settings to environment variables:

```sh
CODEXBAR_PLUGIN_KIRO_GO_USAGE_BASE_URL=https://kiro.example.com \
CODEXBAR_PLUGIN_KIRO_GO_USAGE_API_KEY=your-key \
codexbar plugins fetch kiro-go-usage --json --pretty
```

The API key is sent only as the host-managed `Authorization: Bearer` header;
it is never put into the usage URL by this plugin.
