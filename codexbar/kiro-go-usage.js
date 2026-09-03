defineProvider({
  id: "kiro-go-usage",
  name: "Kiro Go 用量",
  icon: { monogram: "KG", tint: "#D97706" },
  endpoints: [{ setting: "BASE_URL", policy: "https-or-private-network-http" }],
  auth: { type: "bearer", secret: "API_KEY" },
  settings: [
    {
      key: "BASE_URL",
      title: "Kiro Go 服务地址",
      subtitle: "例如 https://kiro.example.com 或 http://127.0.0.1:8080。",
      type: "plain",
    },
    {
      key: "API_KEY",
      title: "API 密钥",
      subtitle: "用于访问 /v1/usage 的个人 Kiro Go API 密钥。",
      type: "secure",
    },
  ],
  capabilities: ["http-status"],

  async fetchUsage(ctx) {
    function fail(message) {
      throw ctx.fail.parseFailure("Kiro Go 用量响应无效：" + message);
    }

    function finite(value, field) {
      if (typeof value !== "number" || !Number.isFinite(value)) fail(field + " 必须是有限数值");
      return value;
    }

    function nonNegativeNumber(value, field) {
      value = finite(value, field);
      if (value < 0) fail(field + " 不能为负数");
      return value;
    }

    function nonNegativeInteger(value, field) {
      value = nonNegativeNumber(value, field);
      if (!Number.isSafeInteger(value)) fail(field + " 必须是安全整数");
      return value;
    }

    function object(value, field) {
      if (!value || typeof value !== "object" || Array.isArray(value)) fail(field + " 必须是对象");
      return value;
    }

    function string(value, field, required) {
      if (value === null || value === undefined) {
        if (required) fail(field + " 为必填项");
        return null;
      }
      if (typeof value !== "string" || (required && value.length === 0)) fail(field + " 必须是字符串");
      return value;
    }

    function dateValue(value, field) {
      if (value === null || value === undefined || value === "" || value === 0) return null;
      if (typeof value === "number") {
        value = finite(value, field);
        if (value < 0) fail(field + " 不能为负数");
        return ctx.date.unixSeconds(value);
      }
      value = string(value, field, true);
      try {
        ctx.date.iso(value);
      } catch (error) {
        fail(field + " 必须是 ISO-8601 日期");
      }
      return value;
    }

    function setting(name, secret) {
      var value = secret ? ctx.settings.getSecret(name) : ctx.settings.get(name);
      if (typeof value !== "string" || value.trim().length === 0) {
        throw ctx.fail.missingCredential(secret ? "尚未配置 Kiro Go API 密钥" : "尚未配置 Kiro Go 服务地址");
      }
      return value.trim();
    }

    var base = setting("BASE_URL", false).replace(/\/+$/, "");
    setting("API_KEY", true);
    if (!/^https?:\/\//i.test(base)) {
      throw ctx.fail.parseFailure("Kiro Go 服务地址必须使用 http:// 或 https://");
    }

    var response;
    try {
      // The natural UTC calendar month is the reporting window; the server
      // clamps an in-progress month at today and reports the next reset.
      response = await ctx.http.getJSON(base + "/v1/usage?month=current", { timeoutSeconds: 15 });
    } catch (error) {
      throw ctx.fail.networkFailure("Kiro Go 用量请求失败：" + ((error && error.message) || error));
    }
    if (response.status === 401) throw ctx.fail.authenticationExpired("Kiro Go 拒绝了该 API 密钥。");
    if (response.status === 403) throw ctx.fail.permissionDenied("该 Kiro Go API 密钥已被禁用。");
    if (response.status === 429) throw ctx.fail.rateLimited("Kiro Go 用量接口返回 HTTP 429。");
    if (response.status >= 500 && response.status <= 599) {
      throw ctx.fail.providerUnavailable("Kiro Go 用量接口返回 HTTP " + response.status + "。");
    }
    if (response.status < 200 || response.status >= 300) {
      throw ctx.fail.apiFailure("Kiro Go 用量接口返回 HTTP " + response.status + "。");
    }

    var data = response.json;
    object(data, "body");
    if (data.timezone !== "UTC") fail("timezone 必须为 UTC");
    if (typeof data.historyAvailableFrom !== "undefined" && data.historyAvailableFrom !== "") {
      var historyDate = string(data.historyAvailableFrom, "historyAvailableFrom", true);
      if (!/^\d{4}-\d{2}-\d{2}$/.test(historyDate)) fail("historyAvailableFrom 必须为 YYYY-MM-DD");
    }
    object(data.range, "range");
    var rangeStart = string(data.range.start, "range.start", true);
    var rangeEnd = string(data.range.end, "range.end", true);
    var rangeDays = nonNegativeInteger(data.range.days, "range.days");
    if (!/^\d{4}-\d{2}-\d{2}$/.test(rangeStart) || !/^\d{4}-\d{2}-\d{2}$/.test(rangeEnd)) {
      fail("range 中的日期必须为 YYYY-MM-DD");
    }
    if (rangeDays < 1 || rangeDays > 366) fail("range.days 必须在 1 到 366 之间");

    // Month metadata is optional so the plugin still works against an older
    // Kiro Go build that only reports a rolling range.
    var rangeKind = string(data.range.kind, "range.kind", false) || "days";
    var monthLabel = string(data.range.month, "range.month", false);
    if (monthLabel && !/^\d{4}-\d{2}$/.test(monthLabel)) fail("range.month 必须为 YYYY-MM");
    var monthStart = string(data.range.monthStart, "range.monthStart", false);
    var monthEnd = string(data.range.monthEnd, "range.monthEnd", false);
    if (monthStart && !/^\d{4}-\d{2}-\d{2}$/.test(monthStart)) fail("range.monthStart 必须为 YYYY-MM-DD");
    if (monthEnd && !/^\d{4}-\d{2}-\d{2}$/.test(monthEnd)) fail("range.monthEnd 必须为 YYYY-MM-DD");
    var monthResetsAt = dateValue(data.range.resetsAt, "range.resetsAt");

    object(data.quotaUsage, "quotaUsage");
    object(data.totals, "totals");
    if (!Array.isArray(data.daily)) fail("daily 必须为数组");
    if (data.daily.length > 120) fail("daily 包含超过 120 个数据点");
    if (data.daily.length !== rangeDays) fail("daily 长度与 range.days 不一致");

    var totals = {
      requests: nonNegativeInteger(data.totals.requests, "totals.requests"),
      inputTokens: nonNegativeInteger(data.totals.inputTokens, "totals.inputTokens"),
      outputTokens: nonNegativeInteger(data.totals.outputTokens, "totals.outputTokens"),
      totalTokens: nonNegativeInteger(data.totals.totalTokens, "totals.totalTokens"),
      credits: nonNegativeNumber(data.totals.credits, "totals.credits"),
    };
    if (totals.inputTokens + totals.outputTokens !== totals.totalTokens) {
      fail("totals.totalTokens 不等于 inputTokens + outputTokens");
    }

    // modelRows validates an optional per-model breakdown attached to a day or
    // to the whole window. Tokens must never exceed the parent scope's total.
    function modelRows(value, field, parentTokens) {
      if (value === null || value === undefined) return [];
      if (!Array.isArray(value)) fail(field + " 必须为数组");
      if (value.length > 64) fail(field + " 包含超过 64 个模型");
      var rows = [];
      var seen = {};
      var tokenSum = 0;
      for (var m = 0; m < value.length; m += 1) {
        var raw = object(value[m], field + "[" + m + "]");
        var name = string(raw.model, field + "[" + m + "].model", true);
        if (name.length > 128) fail(field + "[" + m + "].model 过长");
        if (seen[name]) fail(field + " 包含重复模型 " + name);
        seen[name] = true;
        var modelRow = {
          model: name,
          requests: nonNegativeInteger(raw.requests, field + "[" + m + "].requests"),
          inputTokens: nonNegativeInteger(raw.inputTokens, field + "[" + m + "].inputTokens"),
          outputTokens: nonNegativeInteger(raw.outputTokens, field + "[" + m + "].outputTokens"),
          totalTokens: nonNegativeInteger(raw.totalTokens, field + "[" + m + "].totalTokens"),
          credits: nonNegativeNumber(raw.credits, field + "[" + m + "].credits"),
        };
        if (modelRow.inputTokens + modelRow.outputTokens !== modelRow.totalTokens) {
          fail(field + "[" + m + "].totalTokens 不等于 inputTokens + outputTokens");
        }
        tokenSum += modelRow.totalTokens;
        if (!Number.isSafeInteger(tokenSum)) fail(field + " 的 Token 数超出支持的数值范围");
        rows.push(modelRow);
      }
      if (tokenSum > parentTokens) fail(field + " 的 Token 数超过报告总数");
      return rows;
    }

    var daily = [];
    var dailyTotals = { requests: 0, inputTokens: 0, outputTokens: 0, totalTokens: 0, credits: 0 };
    var previousDate = null;
    for (var i = 0; i < data.daily.length; i += 1) {
      var row = object(data.daily[i], "daily[" + i + "]");
      var date = string(row.date, "daily[" + i + "].date", true);
      if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) fail("daily[" + i + "].date 必须为 YYYY-MM-DD");
      if (i === 0 && date !== rangeStart) fail("daily 未从 range.start 开始");
      if (i === data.daily.length - 1 && date !== rangeEnd) fail("daily 未在 range.end 结束");
      if (previousDate !== null && date <= previousDate) fail("daily 日期必须严格递增");
      var dailyRow = {
        date: date,
        requests: nonNegativeInteger(row.requests, "daily[" + i + "].requests"),
        inputTokens: nonNegativeInteger(row.inputTokens, "daily[" + i + "].inputTokens"),
        outputTokens: nonNegativeInteger(row.outputTokens, "daily[" + i + "].outputTokens"),
        totalTokens: nonNegativeInteger(row.totalTokens, "daily[" + i + "].totalTokens"),
        credits: nonNegativeNumber(row.credits, "daily[" + i + "].credits"),
      };
      if (dailyRow.inputTokens + dailyRow.outputTokens !== dailyRow.totalTokens) {
        fail("daily[" + i + "].totalTokens 不等于 inputTokens + outputTokens");
      }
      dailyRow.models = modelRows(row.models, "daily[" + i + "].models", dailyRow.totalTokens);
      dailyTotals.requests += dailyRow.requests;
      dailyTotals.inputTokens += dailyRow.inputTokens;
      dailyTotals.outputTokens += dailyRow.outputTokens;
      dailyTotals.totalTokens += dailyRow.totalTokens;
      dailyTotals.credits += dailyRow.credits;
      if (!Number.isSafeInteger(dailyTotals.requests) || !Number.isSafeInteger(dailyTotals.inputTokens) ||
          !Number.isSafeInteger(dailyTotals.outputTokens) || !Number.isSafeInteger(dailyTotals.totalTokens) ||
          !Number.isFinite(dailyTotals.credits)) {
        fail("daily 汇总超出支持的数值范围");
      }
      previousDate = date;
      daily.push(dailyRow);
    }
    if (dailyTotals.requests !== totals.requests || dailyTotals.inputTokens !== totals.inputTokens ||
        dailyTotals.outputTokens !== totals.outputTokens || dailyTotals.totalTokens !== totals.totalTokens ||
        dailyTotals.credits !== totals.credits) {
      fail("totals 与 daily 记录合计不一致");
    }

    var quota = data.quotaUsage;
    var windowModels = modelRows(data.models, "models", totals.totalTokens);
    var accountPool = null;
    if (data.accountPool !== null && data.accountPool !== undefined) {
      var rawPool = object(data.accountPool, "accountPool");
      accountPool = {
        creditsUsed: nonNegativeNumber(rawPool.creditsUsed, "accountPool.creditsUsed"),
        creditLimit: nonNegativeNumber(rawPool.creditLimit, "accountPool.creditLimit"),
        creditsRemaining: nonNegativeNumber(rawPool.creditsRemaining, "accountPool.creditsRemaining"),
      };
      if (accountPool.creditLimit <= 0) fail("accountPool.creditLimit 必须大于 0");
      if (accountPool.creditsRemaining > accountPool.creditLimit) {
        fail("accountPool.creditsRemaining 不能超过 creditLimit");
      }
    }
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

    function percentText(used, limit) {
      return numberText(ctx.pct(used, limit), 1) + "%";
    }

    function activeDays(rows) {
      var count = 0;
      for (var d = 0; d < rows.length; d += 1) {
        if (rows[d].requests > 0 || rows[d].totalTokens > 0) count += 1;
      }
      return count;
    }

    function busiestDay(rows) {
      var best = null;
      for (var d = 0; d < rows.length; d += 1) {
        if (best === null || rows[d].totalTokens > best.totalTokens) best = rows[d];
      }
      if (best === null || best.totalTokens === 0) return "暂无用量";
      return best.date + "  " + numberText(best.totalTokens, 0);
    }

    // CodexBar exposes two top-level ratio bars. The aggregate account-pool
    // credit is primary; the personal key quota remains available as secondary.
    var quotaResetsAt = rangeKind === "month" ? monthResetsAt : null;
    var primary = null;
    if (accountPool) {
      var poolUsedForProgress = Math.min(accountPool.creditsUsed, accountPool.creditLimit);
      primary = {
        usedPercent: ctx.pct(poolUsedForProgress, accountPool.creditLimit),
        resetDescription: "号池 Credit：已用 " + numberText(accountPool.creditsUsed, 3) +
          " / " + numberText(accountPool.creditLimit, 3) +
          "，剩余 " + numberText(accountPool.creditsRemaining, 3),
      };
    } else if (creditLimit > 0) {
      primary = {
        usedPercent: ctx.pct(creditUsed, creditLimit),
        resetsAt: quotaResetsAt,
        resetDescription: numberText(creditUsed, 3) + " / " + numberText(creditLimit, 3) + " Credit",
      };
    }
    var secondary = null;
    if (accountPool && creditLimit > 0) {
      secondary = {
        usedPercent: ctx.pct(creditUsed, creditLimit),
        resetsAt: quotaResetsAt,
        resetDescription: "个人 Key Credit：" + numberText(creditUsed, 3) + " / " + numberText(creditLimit, 3),
      };
    } else if (tokenLimit > 0) {
      secondary = {
        usedPercent: ctx.pct(tokenUsed, tokenLimit),
        resetsAt: quotaResetsAt,
        resetDescription: numberText(tokenUsed, 0) + " / " + numberText(tokenLimit, 0) + " Token",
      };
    }

    var windowTitle = rangeKind === "month" && monthLabel
      ? "本月累计（UTC " + monthLabel + "）"
      : "最近 " + rangeDays + " 天（UTC）";

    var overviewRows = [
      { label: "统计区间", value: rangeStart + " 至 " + rangeEnd, secondaryValue: rangeDays + " 天" },
      { label: "请求数", value: numberText(totals.requests, 0) },
      { label: "输入 Token", value: numberText(totals.inputTokens, 0) },
      { label: "输出 Token", value: numberText(totals.outputTokens, 0) },
      { label: "Token 总数", value: numberText(totals.totalTokens, 0) },
      { label: "Credit", value: numberText(totals.credits, 3) },
    ];
    if (rangeKind === "month" && monthResetsAt) {
      overviewRows.push({ label: "月份重置", value: ctx.format.monthDay(monthResetsAt) });
    }

    // Quota rows repeat the ratio as text so the numbers stay readable even
    // when a limit is absent and CodexBar renders no bar at all.
    var quotaRows = [
      {
        label: "Credit",
        value: creditLimit > 0
          ? numberText(creditUsed, 3) + " / " + numberText(creditLimit, 3)
          : numberText(creditUsed, 3),
        secondaryValue: creditLimit > 0 ? "已用 " + percentText(creditUsed, creditLimit) : "无限制",
      },
      {
        label: "Token",
        value: tokenLimit > 0
          ? numberText(tokenUsed, 0) + " / " + numberText(tokenLimit, 0)
          : numberText(tokenUsed, 0),
        secondaryValue: tokenLimit > 0 ? "已用 " + percentText(tokenUsed, tokenLimit) : "无限制",
      },
      { label: "额度请求数", value: numberText(requestsUsed, 0) },
    ];
    if (creditLimit > 0) {
      quotaRows.push({
        label: "剩余 Credit",
        value: numberText(Math.max(0, creditLimit - creditUsed), 3),
        secondaryValue: "剩余 " + numberText(Math.max(0, 100 - ctx.pct(creditUsed, creditLimit)), 1) + "%",
      });
    }
    if (tokenLimit > 0) {
      quotaRows.push({
        label: "剩余 Token",
        value: numberText(Math.max(0, tokenLimit - tokenUsed), 0),
        secondaryValue: "剩余 " + numberText(Math.max(0, 100 - ctx.pct(tokenUsed, tokenLimit)), 1) + "%",
      });
    }
    if (lastResetAt) {
      quotaRows.push({ label: "上次用量重置", value: ctx.format.monthDay(lastResetAt) });
    }

    var tokenPoints = [];
    var creditPoints = [];
    var requestPoints = [];
    for (var j = 0; j < daily.length; j += 1) {
      // Day-of-month labels keep a full month readable on a narrow chart axis.
      var pointLabel = daily[j].date.slice(8);
      tokenPoints.push({ label: pointLabel, value: daily[j].totalTokens });
      creditPoints.push({ label: pointLabel, value: daily[j].credits });
      requestPoints.push({ label: pointLabel, value: daily[j].requests });
    }

    var details = [
      {
        title: windowTitle,
        rows: overviewRows,
        chart: { kind: "line", title: "每日 Token 总数", unit: "Token", points: tokenPoints },
      },
      {
        title: "额度",
        rows: quotaRows,
        chart: { kind: "bars", title: "每日 Credit", unit: "Credit", points: creditPoints },
      },
      {
        title: "每日活动",
        rows: [
          { label: "用量最高日", value: busiestDay(daily) },
          { label: "活跃天数", value: numberText(activeDays(daily), 0) + " / " + rangeDays + " 天" },
          { label: "日均 Token", value: numberText(Math.round(totals.totalTokens / Math.max(1, rangeDays)), 0) },
        ],
        chart: { kind: "bars", title: "每日请求数", unit: "次", points: requestPoints },
      },
    ];

    if (accountPool) {
      details.unshift({
        title: "号池 Credit",
        rows: [
          {
            label: "已用",
            value: numberText(accountPool.creditsUsed, 3) + " Credit",
            secondaryValue: "已用 " + percentText(poolUsedForProgress, accountPool.creditLimit),
          },
          { label: "剩余", value: numberText(accountPool.creditsRemaining, 3) + " Credit" },
          { label: "总量", value: numberText(accountPool.creditLimit, 3) + " Credit" },
        ],
      });
    }

    // The model mix only appears when the server reports attribution, so an
    // older Kiro Go build simply shows one less section.
    if (windowModels.length > 0) {
      var modelDetailRows = [];
      for (var k = 0; k < windowModels.length && k < 12; k += 1) {
        var entry = windowModels[k];
        var share = totals.totalTokens > 0 ? ctx.pct(entry.totalTokens, totals.totalTokens) : 0;
        modelDetailRows.push({
          label: entry.model === "unknown" ? "未知模型" : entry.model,
          value: numberText(share, 1) + "%",
          secondaryValue: numberText(entry.totalTokens, 0) + " Token / " + numberText(entry.requests, 0) + " 次请求",
        });
      }
      details.splice(1, 0, {
        title: "模型占比",
        rows: modelDetailRows,
        chart: {
          kind: "bars",
          title: "各模型 Token 数",
          unit: "Token",
          points: windowModels.slice(0, 12).map(function (entry) {
            return { label: entry.model === "unknown" ? "未知模型" : entry.model, value: entry.totalTokens };
          }),
        },
      });
    }

    return {
      primary: primary,
      secondary: secondary,
      dataConfidence: "estimated",
      details: details,
    };
  },
});
