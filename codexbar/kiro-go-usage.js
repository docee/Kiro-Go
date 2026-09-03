defineProvider({
  id: "kiro-go-usage",
  name: "Kiro Go Usage",
  icon: { monogram: "KG", tint: "#D97706" },
  endpoints: [{ setting: "BASE_URL", policy: "https-or-private-network-http" }],
  auth: { type: "bearer", secret: "API_KEY" },
  settings: [
    {
      key: "BASE_URL",
      title: "Kiro Go base URL",
      subtitle: "For example https://kiro.example.com or http://127.0.0.1:8080.",
      type: "plain",
    },
    {
      key: "API_KEY",
      title: "API key",
      subtitle: "The personal Kiro Go API key used by /v1/usage.",
      type: "secure",
    },
  ],
  capabilities: ["http-status"],

  async fetchUsage(ctx) {
    function fail(message) {
      throw ctx.fail.parseFailure("Invalid Kiro Go usage response: " + message);
    }

    function finite(value, field) {
      if (typeof value !== "number" || !Number.isFinite(value)) fail(field + " must be a finite number");
      return value;
    }

    function nonNegativeNumber(value, field) {
      value = finite(value, field);
      if (value < 0) fail(field + " must be non-negative");
      return value;
    }

    function nonNegativeInteger(value, field) {
      value = nonNegativeNumber(value, field);
      if (!Number.isSafeInteger(value)) fail(field + " must be a safe integer");
      return value;
    }

    function object(value, field) {
      if (!value || typeof value !== "object" || Array.isArray(value)) fail(field + " must be an object");
      return value;
    }

    function string(value, field, required) {
      if (value === null || value === undefined) {
        if (required) fail(field + " is required");
        return null;
      }
      if (typeof value !== "string" || (required && value.length === 0)) fail(field + " must be a string");
      return value;
    }

    function dateValue(value, field) {
      if (value === null || value === undefined || value === "" || value === 0) return null;
      if (typeof value === "number") {
        value = finite(value, field);
        if (value < 0) fail(field + " must be non-negative");
        return ctx.date.unixSeconds(value);
      }
      value = string(value, field, true);
      try {
        ctx.date.iso(value);
      } catch (error) {
        fail(field + " must be an ISO-8601 date");
      }
      return value;
    }

    function setting(name, secret) {
      var value = secret ? ctx.settings.getSecret(name) : ctx.settings.get(name);
      if (typeof value !== "string" || value.trim().length === 0) {
        throw ctx.fail.missingCredential(secret ? "Kiro Go API key is not configured" : "Kiro Go base URL is not configured");
      }
      return value.trim();
    }

    var base = setting("BASE_URL", false).replace(/\/+$/, "");
    setting("API_KEY", true);
    if (!/^https?:\/\//i.test(base)) {
      throw ctx.fail.parseFailure("Kiro Go base URL must use http:// or https://");
    }

    var response;
    try {
      response = await ctx.http.getJSON(base + "/v1/usage?days=30", { timeoutSeconds: 15 });
    } catch (error) {
      throw ctx.fail.networkFailure("Kiro Go usage request failed: " + ((error && error.message) || error));
    }
    if (response.status === 401) throw ctx.fail.authenticationExpired("Kiro Go rejected the API key.");
    if (response.status === 403) throw ctx.fail.permissionDenied("This Kiro Go API key is disabled.");
    if (response.status === 429) throw ctx.fail.rateLimited("Kiro Go usage API returned HTTP 429.");
    if (response.status >= 500 && response.status <= 599) {
      throw ctx.fail.providerUnavailable("Kiro Go usage API returned HTTP " + response.status + ".");
    }
    if (response.status < 200 || response.status >= 300) {
      throw ctx.fail.apiFailure("Kiro Go usage API returned HTTP " + response.status + ".");
    }

    var data = response.json;
    object(data, "body");
    if (data.timezone !== "UTC") fail("timezone must be UTC");
    if (typeof data.historyAvailableFrom !== "undefined" && data.historyAvailableFrom !== "") {
      var historyDate = string(data.historyAvailableFrom, "historyAvailableFrom", true);
      if (!/^\d{4}-\d{2}-\d{2}$/.test(historyDate)) fail("historyAvailableFrom must be YYYY-MM-DD");
    }
    object(data.range, "range");
    var rangeStart = string(data.range.start, "range.start", true);
    var rangeEnd = string(data.range.end, "range.end", true);
    var rangeDays = nonNegativeInteger(data.range.days, "range.days");
    if (!/^\d{4}-\d{2}-\d{2}$/.test(rangeStart) || !/^\d{4}-\d{2}-\d{2}$/.test(rangeEnd)) {
      fail("range dates must be YYYY-MM-DD");
    }
    if (rangeDays < 1 || rangeDays > 366) fail("range.days must be from 1 to 366");
    object(data.quotaUsage, "quotaUsage");
    object(data.totals, "totals");
    if (!Array.isArray(data.daily)) fail("daily must be an array");
    if (data.daily.length > 120) fail("daily contains more than 120 points");
    if (data.daily.length !== rangeDays) fail("daily length does not match range.days");

    var totals = {
      requests: nonNegativeInteger(data.totals.requests, "totals.requests"),
      inputTokens: nonNegativeInteger(data.totals.inputTokens, "totals.inputTokens"),
      outputTokens: nonNegativeInteger(data.totals.outputTokens, "totals.outputTokens"),
      totalTokens: nonNegativeInteger(data.totals.totalTokens, "totals.totalTokens"),
      credits: nonNegativeNumber(data.totals.credits, "totals.credits"),
    };
    if (totals.inputTokens + totals.outputTokens !== totals.totalTokens) {
      fail("totals.totalTokens does not equal inputTokens + outputTokens");
    }

    var daily = [];
    var dailyTotals = { requests: 0, inputTokens: 0, outputTokens: 0, totalTokens: 0, credits: 0 };
    var previousDate = null;
    for (var i = 0; i < data.daily.length; i += 1) {
      var row = object(data.daily[i], "daily[" + i + "]");
      var date = string(row.date, "daily[" + i + "].date", true);
      if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) fail("daily[" + i + "].date must be YYYY-MM-DD");
      if (i === 0 && date !== rangeStart) fail("daily does not start at range.start");
      if (i === data.daily.length - 1 && date !== rangeEnd) fail("daily does not end at range.end");
      if (previousDate !== null && date <= previousDate) fail("daily dates must be strictly ascending");
      var dailyRow = {
        date: date,
        requests: nonNegativeInteger(row.requests, "daily[" + i + "].requests"),
        inputTokens: nonNegativeInteger(row.inputTokens, "daily[" + i + "].inputTokens"),
        outputTokens: nonNegativeInteger(row.outputTokens, "daily[" + i + "].outputTokens"),
        totalTokens: nonNegativeInteger(row.totalTokens, "daily[" + i + "].totalTokens"),
        credits: nonNegativeNumber(row.credits, "daily[" + i + "].credits"),
      };
      if (dailyRow.inputTokens + dailyRow.outputTokens !== dailyRow.totalTokens) {
        fail("daily[" + i + "].totalTokens does not equal inputTokens + outputTokens");
      }
      dailyTotals.requests += dailyRow.requests;
      dailyTotals.inputTokens += dailyRow.inputTokens;
      dailyTotals.outputTokens += dailyRow.outputTokens;
      dailyTotals.totalTokens += dailyRow.totalTokens;
      dailyTotals.credits += dailyRow.credits;
      if (!Number.isSafeInteger(dailyTotals.requests) || !Number.isSafeInteger(dailyTotals.inputTokens) ||
          !Number.isSafeInteger(dailyTotals.outputTokens) || !Number.isSafeInteger(dailyTotals.totalTokens) ||
          !Number.isFinite(dailyTotals.credits)) {
        fail("daily totals exceed supported numeric range");
      }
      previousDate = date;
      daily.push(dailyRow);
    }
    if (dailyTotals.requests !== totals.requests || dailyTotals.inputTokens !== totals.inputTokens ||
        dailyTotals.outputTokens !== totals.outputTokens || dailyTotals.totalTokens !== totals.totalTokens ||
        dailyTotals.credits !== totals.credits) {
      fail("totals do not equal the sum of daily records");
    }

    var quota = data.quotaUsage;
    var creditUsed = nonNegativeNumber(quota.creditsUsed, "quotaUsage.creditsUsed");
    var creditLimit = nonNegativeNumber(quota.creditLimit, "quotaUsage.creditLimit");
    var tokenUsed = nonNegativeInteger(quota.tokensUsed, "quotaUsage.tokensUsed");
    var tokenLimit = nonNegativeInteger(quota.tokenLimit, "quotaUsage.tokenLimit");
    var requestsUsed = nonNegativeInteger(quota.requestsUsed, "quotaUsage.requestsUsed");
    var lastResetAt = dateValue(quota.resetAt, "quotaUsage.resetAt");

    function numberText(value, decimals) {
      if (ctx.format && ctx.format.number) {
        return ctx.format.number(value, { minimumFractionDigits: decimals, maximumFractionDigits: decimals });
      }
      return String(value);
    }

    var primary = null;
    if (creditLimit > 0) {
      primary = {
        usedPercent: ctx.pct(creditUsed, creditLimit),
        resetDescription: numberText(creditUsed, 3) + " / " + numberText(creditLimit, 3) + " credits",
      };
    }
    var secondary = null;
    if (tokenLimit > 0) {
      secondary = {
        usedPercent: ctx.pct(tokenUsed, tokenLimit),
        resetDescription: numberText(tokenUsed, 0) + " / " + numberText(tokenLimit, 0) + " tokens",
      };
    }

    var overviewRows = [
      { label: "Requests", value: numberText(totals.requests, 0) },
      { label: "Input tokens", value: numberText(totals.inputTokens, 0) },
      { label: "Output tokens", value: numberText(totals.outputTokens, 0) },
      { label: "Total tokens", value: numberText(totals.totalTokens, 0) },
      { label: "Credits", value: numberText(totals.credits, 3) },
      { label: "Quota requests", value: numberText(requestsUsed, 0) },
    ];
    if (lastResetAt) {
      overviewRows.push({ label: "Last quota reset", value: ctx.format.monthDay(lastResetAt) });
    }
    var tokenPoints = [];
    var creditPoints = [];
    for (var j = 0; j < daily.length; j += 1) {
      tokenPoints.push({ label: daily[j].date, value: daily[j].totalTokens });
      creditPoints.push({ label: daily[j].date, value: daily[j].credits });
    }

    return {
      primary: primary,
      secondary: secondary,
      dataConfidence: "estimated",
      details: [
        {
          title: "Last 30 days (UTC)",
          rows: overviewRows,
          chart: { kind: "line", title: "Daily total tokens", unit: "tokens", points: tokenPoints },
        },
        {
          title: "Credits",
          rows: [{ label: "Daily credits", value: numberText(totals.credits, 3) }],
          chart: { kind: "bars", title: "Daily credits", unit: "credits", points: creditPoints },
        },
      ],
    };
  },
});
