(() => {
  'use strict';

  const queryParams = new URLSearchParams(window.location.search);
  const hashParams = new URLSearchParams(window.location.hash.replace(/^#\??/, ''));
  const queryApiKey = queryParams.get('api_key') || '';
  const apiKey = hashParams.get('api_key') || queryApiKey;
  if (queryApiKey) {
    // Keep the key client-side and refreshable without sending it in HTTP URLs.
    window.history.replaceState(
      {},
      document.title,
      window.location.pathname + '#api_key=' + encodeURIComponent(queryApiKey)
    );
  }

  const $ = (id) => document.getElementById(id);
  const state = $('state');
  const content = $('content');
  let activeQuery = { days: 30 };
  let latestData = null;

  const number = (value, digits = 0) => {
    const n = Number(value);
    if (!Number.isFinite(n)) return '0';
    return n.toLocaleString(undefined, { maximumFractionDigits: digits });
  };
  const credit = (value) => number(value, 6);
  const dateText = (value) => {
    if (value === null || value === undefined || value === '') return '-';
    if (typeof value === 'number' || /^\d+$/.test(String(value))) {
      const timestamp = Number(value);
      if (Number.isFinite(timestamp)) {
        const date = new Date(timestamp < 100000000000 ? timestamp * 1000 : timestamp);
        if (!Number.isNaN(date.getTime())) return date.toISOString().slice(0, 10);
      }
    }
    return String(value).slice(0, 10);
  };
  const setText = (id, value) => { const node = $(id); if (node) node.textContent = value; };
  const dayRows = (data) => Array.isArray(data.daily) ? data.daily : [];
  const showState = (title, copy, kind = '') => {
    state.className = 'state-panel visible' + (kind ? ' ' + kind : '');
    state.innerHTML = '<div class="state-title">' + title + '</div><p class="state-copy">' + copy + '</p>';
    content.hidden = true;
  };
  const showLoading = () => showState('Loading usage', 'Fetching the latest metering data...');
  const queryString = () => activeQuery.days ? '?days=' + activeQuery.days : '?start=' + encodeURIComponent(activeQuery.start) + '&end=' + encodeURIComponent(activeQuery.end);

  function renderQuota(quota) {
    quota = quota || {};
    const tokenLimit = Number(quota.tokenLimit);
    const creditLimit = Number(quota.creditLimit);
    const tokenUsed = Number(quota.tokensUsed) || 0;
    const creditUsed = Number(quota.creditsUsed) || 0;
    const tokenLimited = Number.isFinite(tokenLimit) && tokenLimit > 0;
    const creditLimited = Number.isFinite(creditLimit) && creditLimit > 0;
    setText('tokensUsed', number(tokenUsed)); setText('tokensLimit', tokenLimited ? number(quota.tokensRemaining) + ' remaining' : 'Unlimited');
    setText('creditsUsed', credit(creditUsed)); setText('creditsLimit', creditLimited ? credit(quota.creditsRemaining) + ' remaining' : 'Unlimited');
    setText('requestsUsed', number(quota.requestsUsed)); setText('resetAt', quota.resetAt ? 'Last reset ' + dateText(quota.resetAt) : 'Not reset yet');
    const tokenRatio = tokenLimited ? Math.min(100, tokenUsed / tokenLimit * 100) : 0;
    const creditRatio = creditLimited ? Math.min(100, creditUsed / creditLimit * 100) : 0;
    $('tokenProgress').style.width = tokenRatio + '%'; $('creditProgress').style.width = creditRatio + '%';
    setText('tokenPercent', tokenLimited ? number(tokenRatio, 1) + '%' : 'No limit'); setText('creditPercent', creditLimited ? number(creditRatio, 1) + '%' : 'No limit');
  }
  function renderData(data) {
    latestData = data || {}; const key = latestData.apiKey || {}; const totals = latestData.totals || {};
    state.className = 'state-panel'; state.innerHTML = ''; content.hidden = false;
    setText('keyIdentity', key.name || key.keyMasked || 'API key usage'); setText('accountName', key.name || key.id || 'Personal API key'); setText('accountKey', key.keyMasked || '');
    setText('historyFrom', dateText(latestData.historyAvailableFrom)); setText('createdAt', dateText(key.createdAt)); setText('lastUsed', key.lastUsedAt ? dateText(key.lastUsedAt) : 'Not used yet');
    setText('rangeLabel', latestData.range && latestData.range.start ? dateText(latestData.range.start) + ' - ' + dateText(latestData.range.end) : 'Last ' + (activeQuery.days || 0) + ' days');
    setText('totalDays', number(latestData.range && latestData.range.days || dayRows(latestData).length) + ' days');
    setText('totalRequests', number(totals.requests)); setText('totalInput', number(totals.inputTokens)); setText('totalOutput', number(totals.outputTokens)); setText('totalTokens', number(totals.totalTokens)); setText('totalCredits', credit(totals.credits));
    renderQuota(latestData.quotaUsage); renderTable(dayRows(latestData)); drawChart(dayRows(latestData));
  }
  function renderTable(rows) {
    const body = $('dailyRows');
    if (!rows.length) { body.innerHTML = '<tr><td colspan="6" class="empty-cell">No usage in this range</td></tr>'; return; }
    body.innerHTML = rows.map((row) => '<tr><td>' + dateText(row.date) + '</td><td>' + number(row.requests) + '</td><td>' + number(row.inputTokens) + '</td><td>' + number(row.outputTokens) + '</td><td>' + number(row.totalTokens) + '</td><td>' + credit(row.credits) + '</td></tr>').join('');
  }
  function drawChart(rows) {
    const canvas = $('usageChart'); const empty = $('chartEmpty');
    const hasData = rows.some((row) => Number(row.totalTokens) || Number(row.credits)); empty.hidden = hasData;
    const rect = canvas.getBoundingClientRect(); const ratio = window.devicePixelRatio || 1; const width = Math.max(1, rect.width); const height = Math.max(1, rect.height);
    canvas.width = Math.round(width * ratio); canvas.height = Math.round(height * ratio); const ctx = canvas.getContext('2d'); ctx.setTransform(ratio, 0, 0, ratio, 0, 0); ctx.clearRect(0, 0, width, height); if (!rows.length || !hasData) return;
    const pad = { top:12, right:10, bottom:28, left:42 }; const chartW = width - pad.left - pad.right; const chartH = height - pad.top - pad.bottom; const maxToken = Math.max(1, ...rows.map((row) => Number(row.totalTokens) || 0)); const maxCredit = Math.max(1, ...rows.map((row) => Number(row.credits) || 0));
    ctx.strokeStyle = '#e9ebef'; ctx.lineWidth = 1; ctx.fillStyle = '#8a909c'; ctx.font = '10px Geist Mono, monospace'; ctx.textAlign = 'right';
    for (let i = 0; i < 4; i += 1) { const y = pad.top + chartH * i / 3; ctx.beginPath(); ctx.moveTo(pad.left, y + .5); ctx.lineTo(width - pad.right, y + .5); ctx.stroke(); ctx.fillText(number(maxToken * (1 - i / 3)), pad.left - 8, y + 3); }
    const point = (index, value, max) => [pad.left + (rows.length === 1 ? chartW / 2 : chartW * index / (rows.length - 1)), pad.top + chartH - chartH * value / max];
    const line = (color, field, max) => { ctx.strokeStyle = color; ctx.lineWidth = 2; ctx.beginPath(); rows.forEach((row, index) => { const p = point(index, Number(row[field]) || 0, max); index ? ctx.lineTo(p[0], p[1]) : ctx.moveTo(p[0], p[1]); }); ctx.stroke(); rows.forEach((row, index) => { const p = point(index, Number(row[field]) || 0, max); ctx.fillStyle = color; ctx.beginPath(); ctx.arc(p[0], p[1], 2.7, 0, Math.PI * 2); ctx.fill(); }); };
    line('#315bef', 'totalTokens', maxToken); line('#0f8b83', 'credits', maxCredit);
    ctx.fillStyle = '#8a909c'; ctx.textAlign = 'center'; const labels = Math.min(6, rows.length); for (let i = 0; i < labels; i += 1) { const index = Math.round(i * (rows.length - 1) / Math.max(1, labels - 1)); const p = point(index, 0, maxToken); ctx.fillText(dateText(rows[index].date).slice(5), p[0], height - 8); }
  }
  async function fetchUsage() {
    if (!apiKey) { showState('API key required', 'Open this page with an api_key query parameter.', 'error'); return; }
    showLoading();
    try {
      const response = await fetch('/v1/usage' + queryString(), { headers: { Authorization: 'Bearer ' + apiKey }, cache: 'no-store' });
      if (response.status === 401) throw new Error('401'); if (response.status === 403) throw new Error('403'); if (!response.ok) throw new Error('server');
      renderData(await response.json());
    } catch (error) {
      if (error.message === '401') showState('Invalid API key', 'The key was not recognized. Check the link and try again.', 'error');
      else if (error.message === '403') showState('API key disabled', 'This API key is disabled and cannot be queried.', 'error');
      else showState('Unable to load usage', 'Check your connection and try refreshing the page.', 'error');
    }
  }
  function setPreset(days, button) { activeQuery = { days }; document.querySelectorAll('.segmented button').forEach((node) => node.classList.toggle('active', node === button)); $('startDate').value = ''; $('endDate').value = ''; fetchUsage(); }
  document.querySelectorAll('.segmented button').forEach((button) => button.addEventListener('click', () => setPreset(Number(button.dataset.days), button)));
  $('customRange').addEventListener('submit', (event) => { event.preventDefault(); const start = $('startDate').value; const end = $('endDate').value; if (!start || !end || start > end) { showState('Invalid date range', 'Choose a UTC start date on or before the end date.', 'error'); return; } const startMs = Date.parse(start + 'T00:00:00Z'); const endMs = Date.parse(end + 'T00:00:00Z'); if ((endMs - startMs) / 86400000 > 365) { showState('Range is too long', 'Choose a range of 366 days or fewer.', 'error'); return; } activeQuery = { start, end }; document.querySelectorAll('.segmented button').forEach((node) => node.classList.remove('active')); fetchUsage(); });
  $('refreshButton').addEventListener('click', fetchUsage); window.addEventListener('resize', () => { if (latestData) drawChart(dayRows(latestData)); });
  fetchUsage();
})();
