# Kiro Go 用量 CodexBar 插件

`kiro-go-usage.js` is a local CodexBar provider for the Kiro Go personal usage
endpoint. It requests `GET /v1/usage?month=current`, keeps the API key in
CodexBar's secure settings, and reports credit and token quota windows plus
daily charts.
Credit values are Kiro's internal metering units, not currency. The plugin marks
the snapshot as `dataConfidence: "estimated"` because upstream token reporting
can be estimated for some requests.

## What it reports

The window is the current natural UTC calendar month, not a rolling 30 days.
A month in progress ends at the current UTC day, and the plugin shows when the
month rolls over. Sections are:

1. Month to date: window bounds, requests, tokens, credits, and month reset.
2. Account-pool credit: aggregate used, remaining, and total main-credit
   capacity in the primary progress bar; no account identities or per-account values.
3. Model mix: each model's share of window tokens, largest first.
4. Quota: used/limit with the remaining amount and percentage.
5. Daily activity: busiest day, active days, and average daily tokens.

When `tokenLimit` or `creditLimit` is set on the key, the plugin returns those
as rate windows so CodexBar draws its percentage bars and ties them to the
month rollover. A key with no limits reports usage totals without bars, since
a ratio has no meaning without a denominator.

Model attribution comes from the server's per-model daily ledger. Requests
recorded before that ledger existed, or arriving without a usable model name,
are grouped under `unknown`. Against an older Kiro Go build that reports
neither month metadata nor model rows, the plugin falls back to labeling the
window by day count and omits the model section.
The account-pool section is also optional and is hidden when the server does not
report any enabled account with a finite main-credit quota. Trial credits are
not included.

## 安装与初始化

1. 将 `kiro-go-usage.js` 放入 `~/.config/codexbar/providers/`，或在
   CodexBar 的 **设置 -> 插件 -> 安装** 中选择该文件。
2. 首次配置时直接填写 `BASE_URL` 和 `API_KEY`。`BASE_URL` 只填写
   Kiro Go 服务地址，例如 `https://kiro.example.com`，无需附加
   `/v1/usage`；`API_KEY` 填写 Kiro Go 中配置的个人 API 密钥。
3. CodexBar 会对实际网络来源和 Bearer 认证进行一次宿主级安全批准。这是
   CodexBar 对所有本地联网插件的强制保护，插件 API 不支持跳过。

本地 HTTP 服务应使用回环地址、RFC 1918、链路本地地址、唯一本地地址或
`.local` 地址。公网服务必须使用 HTTPS。

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

## Related server parameters

`GET /v1/usage` accepts `month=YYYY-MM` or `month=current` for natural UTC
months. The older `days=N` and `start`/`end` parameters still work and keep
their rolling behavior, so existing callers are unaffected; a request with no
parameters now defaults to the current month.
