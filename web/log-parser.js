// ===== 调用日志内容解析引擎（v1.0.0） =====
// 纯函数模块：将日志记录的请求体 / 响应体（含 SSE 流）解析为结构化中间表示，
// 不操作 DOM；渲染由 views-logs.js 完成。覆盖三种接口格式：
//   cc        OpenAI Chat Completions（/v1/chat/completions）
//   responses OpenAI Responses API（/v1/responses）
//   anthropic Anthropic Messages API（/v1/messages）
//
// 解析原则（完整性对账）：已知字段结构化展示，未知字段收入 other
// （渲染层放入「其他字段」折叠区），两级合计覆盖原始 JSON 的全部 key，不丢弃任何字段。
const LogParser = (() => {
  'use strict';

  const TRUNC_MARK = '...(truncated)';

  const FORMAT_LABELS = {
    cc: 'OpenAI Chat Completions',
    responses: 'OpenAI Responses',
    anthropic: 'Anthropic Messages',
    unknown: '未识别格式',
  };

  // 请求体顶层参数白名单（结构化 kv 展示）；不在白名单且非结构字段的一律进 other
  const CC_PARAM_KEYS = [
    'stream', 'stream_options', 'temperature', 'top_p', 'max_tokens', 'max_completion_tokens',
    'max_output_tokens', 'stop', 'stop_sequences', 'tool_choice', 'response_format',
    'reasoning_effort', 'reasoning', 'seed', 'user', 'n', 'parallel_tool_calls',
    'presence_penalty', 'frequency_penalty', 'logprobs', 'top_logprobs', 'logit_bias',
    'modalities', 'audio', 'prediction', 'service_tier', 'store', 'metadata',
    'web_search_options', 'text', 'functions', 'function_call', 'enable_thinking', 'thinking',
    'chat_template_kwargs', 'safety_identifier', 'prompt_cache_key', 'deployment',
  ];
  // 三格式共用的结构字段（由消息/工具/参数区消费，不进 other）
  const STRUCT_KEYS = {
    cc: ['model', 'messages', 'tools'],
    anthropic: ['model', 'messages', 'tools', 'system'],
    responses: ['model', 'input', 'tools', 'instructions'],
  };

  // ---------- 基础工具 ----------

  function isTruncated(s) {
    return typeof s === 'string' && s.length > TRUNC_MARK.length &&
      s.slice(-TRUNC_MARK.length) === TRUNC_MARK;
  }

  function safeParse(s) {
    try { return { obj: JSON.parse(s), ok: true }; }
    catch (e) { return { obj: null, ok: false }; }
  }

  function str(v) { return v == null ? '' : String(v); }

  function b64KB(data) {
    if (typeof data !== 'string' || !data) return null;
    return Math.max(1, Math.round(data.length * 3 / 4 / 1024));
  }

  // ---------- SSE ----------

  // 判定 body 是否为 SSE 事件流：首个非空白内容以 data: / event: / 注释行开头。
  // 非流式 JSON 以 { 开头、HTML 错误页以 < 开头，均不会误判。
  function looksSSE(s) {
    if (typeof s !== 'string' || s === '') return false;
    const t = s.replace(/^\uFEFF/, '').trimStart();
    return t.startsWith('data:') || t.startsWith('event:') || t.startsWith(':');
  }

  // 解析 SSE 文本 → 帧列表。按规范：事件以空行分隔，data: 行可多行拼接；
  // 注释行（: 开头）忽略；末尾无空行的残留帧（截断常见）也结算。
  function parseSSE(text) {
    const frames = [];
    let bad = 0, done = false;
    if (typeof text !== 'string' || text === '') return { frames, bad, done };
    let dataLines = [], event = null, sawData = false;
    const flush = () => {
      if (!sawData) { dataLines = []; event = null; return; }
      const data = dataLines.join('\n');
      if (data === '[DONE]') { done = true; }
      else {
        let parsed = null;
        try { parsed = JSON.parse(data); } catch (e) { bad++; }
        frames.push({ event, data, parsed });
      }
      dataLines = []; event = null; sawData = false;
    };
    for (const raw of text.split('\n')) {
      const line = raw.replace(/\r$/, '');
      if (line === '') { flush(); continue; }
      if (line.startsWith(':')) continue; // 注释 / keep-alive
      if (line.startsWith('data:')) { dataLines.push(line.slice(5).replace(/^ /, '')); sawData = true; }
      else if (line.startsWith('event:')) { event = line.slice(6).replace(/^ /, ''); }
      // 其余字段（id:/retry:）忽略
    }
    flush();
    return { frames, bad, done };
  }

  // ---------- 格式识别 ----------

  // URL 路径优先；body 结构兜底。四组方向数据各自独立调用（跨格式时两侧格式不同）。
  // v1.0.0（F5）：末段精确匹配优先——includes 子串顺序匹配会被 base_url 干扰
  //（如 Anthropic 供应商 base_url 含 /responses 子串时，完整上游 URL 会先命中
  // responses 分支误判，且 URL 命中后无帧内容兜底）；末段匹配覆盖正常场景，
  // 子串 includes 仅作兕底。
  function detectFormat(url, bodyStr, frames) {
    const u = str(url).toLowerCase();
    if (u.endsWith('/chat/completions')) return 'cc';
    if (u.endsWith('/responses')) return 'responses';
    if (u.endsWith('/messages')) return 'anthropic';
    if (u.includes('/chat/completions')) return 'cc';
    if (u.includes('/messages')) return 'anthropic';
    if (u.includes('/responses')) return 'responses';
    if (frames) return guessFormatFromFrames(frames);
    return guessFormatFromJSON(safeParse(str(bodyStr)).obj);
  }

  function guessFormatFromFrames(frames) {
    for (const f of frames || []) {
      if (!f.parsed) continue;
      const d = f.parsed;
      if (Array.isArray(d.choices)) return 'cc';
      if (typeof d.type === 'string') {
        if (d.type.startsWith('response.')) return 'responses';
        if (d.type.startsWith('content_block_') || d.type.startsWith('message_')) return 'anthropic';
      }
    }
    return 'unknown';
  }

  function guessFormatFromJSON(obj) {
    if (!obj || typeof obj !== 'object' || Array.isArray(obj)) return 'unknown';
    if (Array.isArray(obj.choices)) return 'cc';
    // v1.0.0（F4）：Responses 的 input 允许纯字符串（单条 user 文本，与代理后端解析器同步），
    // URL 缺失的旧日志/未发上游的失败记录只能靠 JSON 兕底猜测时不再落入 unknown
    if (Array.isArray(obj.output) || Array.isArray(obj.input) || typeof obj.input === 'string' || 'instructions' in obj) return 'responses';
    if (obj.type === 'message' && Array.isArray(obj.content)) return 'anthropic';
    if (Array.isArray(obj.messages)) {
      // Anthropic 特征：顶层 system / stop_sequences / tool_use、tool_result、thinking 块
      if ('system' in obj || Array.isArray(obj.stop_sequences)) return 'anthropic';
      const hasAnthropicBlock = obj.messages.some(m => Array.isArray(m.content) && m.content.some(c =>
        c && typeof c === 'object' && ['tool_use', 'tool_result', 'thinking', 'redacted_thinking'].includes(c.type)));
      if (hasAnthropicBlock) return 'anthropic';
      return 'cc';
    }
    return 'unknown';
  }

  // ---------- 通用归一化 ----------

  function normError(err) {
    if (err == null) return null;
    if (typeof err === 'string') return { message: err, type: null };
    if (typeof err !== 'object') return { message: String(err), type: null };
    let message = err.message != null ? String(err.message) : (err.msg != null ? String(err.msg) : '');
    if (!message) { try { message = JSON.stringify(err); } catch (e) { message = String(err); } }
    return {
      message,
      type: err.type != null ? String(err.type) : (err.code != null ? String(err.code) : (err.name || null)),
      param: err.param != null ? String(err.param) : null,
    };
  }

  // usage 对象 → [中文标签, 值] 行列表；有值才显示；未知细分字段兜底收录
  function normalizeUsage(u) {
    if (!u || typeof u !== 'object') return [];
    const rows = [];
    const push = (k, v) => { if (v != null && v !== '') rows.push([k, String(v)]); };
    push('输入 Token', u.prompt_tokens != null ? u.prompt_tokens : u.input_tokens);
    push('输出 Token', u.completion_tokens != null ? u.completion_tokens : u.output_tokens);
    push('总 Token', u.total_tokens);
    const inD = u.prompt_tokens_details || u.input_tokens_details;
    if (inD) push('输入·缓存命中', inD.cached_tokens);
    const outD = u.completion_tokens_details || u.output_tokens_details;
    if (outD) {
      push('输出·推理', outD.reasoning_tokens);
      push('输出·音频', outD.audio_tokens);
      push('输出·已接受', outD.accepted_tokens);
      push('输出·被拒绝', outD.rejected_tokens);
    }
    const known = ['prompt_tokens', 'completion_tokens', 'total_tokens', 'input_tokens', 'output_tokens',
      'prompt_tokens_details', 'completion_tokens_details', 'input_tokens_details', 'output_tokens_details'];
    for (const k of Object.keys(u)) {
      if (!known.includes(k) && u[k] != null && typeof u[k] !== 'object') rows.push([k, String(u[k])]);
    }
    return rows;
  }

  // ---------- 请求体：内容段归一化 ----------

  // CC content part（text / image_url / input_audio / file / thinking…）
  function normCCContentPart(c) {
    if (c == null) return { t: 'raw', label: 'null', value: null };
    if (typeof c === 'string') return { t: 'text', text: c }; // 宽容：个别网关用字符串
    switch (c.type) {
      case 'text': return { t: 'text', text: str(c.text) };
      case 'image_url': {
        const iu = c.image_url || {};
        const url = typeof iu === 'string' ? iu : str(iu.url);
        const base64 = url.startsWith('data:');
        return {
          t: 'image', url, detail: iu.detail, base64,
          dataUrl: base64 ? url : null,
          mime: base64 ? url.slice(5, url.indexOf(';')) : null,
          kb: base64 ? b64KB(url.slice(url.indexOf(',') + 1)) : null,
        };
      }
      case 'input_audio': {
        const a = c.input_audio || {};
        return { t: 'audio', format: a.format, kb: b64KB(a.data) };
      }
      case 'file': {
        const f = c.file || {};
        return { t: 'file', name: f.filename || f.file_id, mime: null, kb: b64KB(f.file_data) };
      }
      case 'thinking': return { t: 'thinking', text: str(c.thinking || c.text) };
      default: return { t: 'raw', label: c.type || '未知类型', value: c };
    }
  }

  // Anthropic content block（text / thinking / tool_use / tool_result / image / document…）
  function normAnthropicBlock(b) {
    if (b == null) return { t: 'raw', label: 'null', value: null };
    switch (b.type) {
      case 'text': return { t: 'text', text: str(b.text) };
      case 'thinking': return { t: 'thinking', text: str(b.thinking) };
      case 'redacted_thinking': return { t: 'thinking', text: '(已加密的推理内容，不可读)', redacted: true };
      case 'tool_use': return {
        t: 'toolcall', id: str(b.id), name: str(b.name),
        args: b.input != null ? JSON.stringify(b.input, null, 2) : '',
      };
      case 'tool_result': {
        const out = { t: 'toolresult', id: str(b.tool_use_id), isError: !!b.is_error, text: null, parts: [] };
        if (typeof b.content === 'string') out.text = b.content;
        else if (Array.isArray(b.content)) out.parts = b.content.map(normAnthropicBlock);
        else if (b.content != null) out.text = JSON.stringify(b.content, null, 2);
        return out;
      }
      case 'image': {
        const s = b.source || {};
        if (s.type === 'base64') {
          return {
            t: 'image', base64: true, mime: s.media_type, kb: b64KB(s.data),
            dataUrl: 'data:' + str(s.media_type) + ';base64,' + str(s.data),
          };
        }
        if (s.type === 'url') return { t: 'image', url: str(s.url) };
        return { t: 'raw', label: 'image', value: b };
      }
      case 'document': {
        const s = b.source || {};
        return { t: 'file', name: b.title, mime: s.media_type, kb: s.type === 'base64' ? b64KB(s.data) : null };
      }
      default: return { t: 'raw', label: b.type || '未知类型', value: b };
    }
  }

  // Responses content part（input_text / output_text / input_image / input_audio / refusal…）
  function normResponsesContentPart(c) {
    if (c == null) return { t: 'raw', label: 'null', value: null };
    if (typeof c === 'string') return { t: 'text', text: c };
    switch (c.type) {
      case 'input_text': case 'output_text': case 'text': case 'summary_text':
        return { t: 'text', text: str(c.text) };
      case 'input_image': {
        const url = typeof c.image_url === 'string' ? c.image_url : str(c.image_url);
        const base64 = url.startsWith('data:');
        return {
          t: 'image', url, detail: c.detail, base64,
          dataUrl: base64 ? url : null,
          mime: base64 ? url.slice(5, url.indexOf(';')) : null,
          kb: base64 ? b64KB(url.slice(url.indexOf(',') + 1)) : null,
        };
      }
      case 'input_audio': {
        const a = c.input_audio || {};
        return { t: 'audio', format: a.format, kb: b64KB(a.data) };
      }
      case 'refusal': return { t: 'raw', label: 'refusal', value: str(c.refusal) };
      default: return { t: 'raw', label: c.type || '未知类型', value: c };
    }
  }

  // Responses 条目（message / function_call / function_call_output / reasoning / 其他工具条目）
  // → 统一为消息卡片形态 { role, parts, extra }
  function normResponsesItem(it) {
    if (it == null || typeof it !== 'object') return { role: 'unknown', parts: [], extra: { itemType: 'null' } };
    switch (it.type) {
      case 'message': {
        const parts = [];
        if (typeof it.content === 'string') parts.push({ t: 'text', text: it.content });
        else if (Array.isArray(it.content)) parts.push(...it.content.map(normResponsesContentPart));
        else if (it.content != null) parts.push({ t: 'raw', label: 'content', value: it.content });
        return { role: it.role || 'unknown', parts, extra: { itemType: 'message', status: it.status } };
      }
      case 'function_call': return {
        role: 'assistant',
        parts: [{ t: 'toolcall', id: str(it.call_id || it.id), name: str(it.name), args: str(it.arguments) }],
        extra: { itemType: 'function_call', status: it.status },
      };
      case 'function_call_output': {
        let text = null;
        if (typeof it.output === 'string') text = it.output;
        else if (it.output != null) { try { text = JSON.stringify(it.output, null, 2); } catch (e) { text = String(it.output); } }
        return { role: 'tool', parts: [{ t: 'toolresult', id: str(it.call_id), text }], extra: { itemType: 'function_call_output' } };
      }
      case 'reasoning': {
        const parts = [];
        if (Array.isArray(it.summary)) {
          for (const s of it.summary) {
            if (s && typeof s === 'object' && (s.type === 'summary_text' || s.text != null)) parts.push({ t: 'thinking', text: str(s.text) });
            else parts.push({ t: 'raw', label: (s && s.type) || 'summary', value: s });
          }
        }
        if (Array.isArray(it.content)) for (const c of it.content) parts.push(normResponsesContentPart(c));
        return { role: 'reasoning', parts, extra: { itemType: 'reasoning' } };
      }
      case 'item_reference': return { role: 'reference', parts: [{ t: 'raw', label: 'item_reference', value: it }], extra: { itemType: 'item_reference' } };
      default: return { role: 'tool', parts: [{ t: 'raw', label: it.type || '未知条目', value: it }], extra: { itemType: it.type } };
    }
  }

  // ---------- 请求体：消息归一化 ----------

  function normCCMessages(messages) {
    const out = [];
    for (const m of (messages || [])) {
      if (m == null || typeof m !== 'object') { out.push({ role: 'unknown', parts: [{ t: 'raw', label: 'message', value: m }], extra: {} }); continue; }
      const role = str(m.role) || 'unknown';

      // role=tool：整条消息作为「工具结果」渲染（tool_call_id + content，含 JSON 格式化）
      if (role === 'tool' && m.content != null) {
        const tr = { t: 'toolresult', id: str(m.tool_call_id), isError: !!m.is_error, text: null, parts: [] };
        if (typeof m.content === 'string') tr.text = m.content;
        else if (Array.isArray(m.content)) tr.parts = m.content.map(normCCContentPart);
        else tr.text = JSON.stringify(m.content, null, 2);
        out.push({ role: 'tool', parts: [tr], extra: {} });
        continue;
      }

      const parts = [];
      if (typeof m.content === 'string') { if (m.content) parts.push({ t: 'text', text: m.content }); }
      else if (Array.isArray(m.content)) parts.push(...m.content.map(normCCContentPart));
      else if (m.content != null) parts.push({ t: 'raw', label: 'content', value: m.content });
      if (typeof m.reasoning_content === 'string' && m.reasoning_content) {
        parts.push({ t: 'thinking', text: m.reasoning_content, label: 'reasoning_content' });
      }
      if (Array.isArray(m.tool_calls)) {
        for (const tc of m.tool_calls) {
          const fn = (tc && tc.function) || {};
          const args = typeof fn.arguments === 'string' ? fn.arguments :
            (fn.arguments != null ? JSON.stringify(fn.arguments) : '');
          parts.push({ t: 'toolcall', id: str(tc && tc.id), name: str(fn.name) || '(未命名)', args, type: str(tc && tc.type) });
        }
      }
      const extra = {};
      if (m.tool_call_id != null) extra.toolCallId = str(m.tool_call_id);
      if (m.name != null) extra.fnName = str(m.name); // 旧版 function calling 的 name
      out.push({ role, parts, extra });
    }
    return out;
  }

  function normAnthropicMessages(messages) {
    const out = [];
    for (const m of (messages || [])) {
      if (m == null || typeof m !== 'object') { out.push({ role: 'unknown', parts: [{ t: 'raw', label: 'message', value: m }], extra: {} }); continue; }
      const parts = [];
      if (typeof m.content === 'string') { if (m.content) parts.push({ t: 'text', text: m.content }); }
      else if (Array.isArray(m.content)) parts.push(...m.content.map(normAnthropicBlock));
      else if (m.content != null) parts.push({ t: 'raw', label: 'content', value: m.content });
      out.push({ role: str(m.role) || 'unknown', parts, extra: {} });
    }
    return out;
  }

  function normResponsesInput(input) {
    // input 可为数组（条目列表）或字符串（单条 user 文本）
    if (typeof input === 'string') return [{ role: 'user', parts: [{ t: 'text', text: input }], extra: { itemType: 'text' } }];
    if (!Array.isArray(input)) return [];
    return input.map(normResponsesItem);
  }

  // ---------- 请求体：工具定义归一化 ----------

  function normTools(tools, format) {
    const out = [];
    for (const t of (tools || [])) {
      if (t == null || typeof t !== 'object') { out.push({ name: '(无效条目)', type: null, description: null, schema: null, raw: t }); continue; }
      if (format === 'anthropic') {
        out.push({ name: str(t.name) || '(未命名)', type: t.type || 'custom', description: t.description, schema: t.input_schema, raw: t });
      } else if (format === 'responses') {
        out.push({ name: str(t.name) || str(t.type) || '(未命名)', type: t.type || 'function', description: t.description, schema: t.parameters, raw: t });
      } else {
        // CC：{type:'function', function:{name, description, parameters}}；宽容顶层平铺
        const fn = t.function || t;
        out.push({
          name: str(fn.name) || str(t.name) || '(未命名)',
          type: t.type || 'function',
          description: fn.description != null ? fn.description : t.description,
          schema: fn.parameters != null ? fn.parameters : t.parameters,
          raw: t,
        });
      }
    }
    return out;
  }

  // ---------- 请求体解析主入口 ----------

  // 返回 { ok: true, format, model, params, messages, tools, other } 或 { ok: false, reason, format }
  // reason: 'empty' | 'non-json' | 'truncated'
  function parseRequest(bodyStr, url) {
    const body = typeof bodyStr === 'string' ? bodyStr : '';
    if (!body) return { ok: false, reason: 'empty', format: detectFormat(url, '', null) };
    const truncated = isTruncated(body);
    const { obj, ok } = safeParse(body);
    if (!ok) return { ok: false, reason: truncated ? 'truncated' : 'non-json', format: detectFormat(url, '', null) };
    const format = detectFormat(url, body, null);
    if (format === 'unknown') return { ok: false, reason: 'unknown-format', format };

    const out = { ok: true, format, model: null, params: [], messages: [], tools: [], other: {} };
    const consumed = {}; // 已消费的顶层 key（结构字段 + 参数白名单）
    const consume = keys => { for (const k of keys) consumed[k] = true; };

    out.model = obj.model != null ? String(obj.model) : null;

    if (format === 'cc') {
      if (obj.messages != null) out.messages = normCCMessages(obj.messages);
      if (obj.tools != null) out.tools = normTools(obj.tools, 'cc');
      consume(STRUCT_KEYS.cc);
      for (const k of CC_PARAM_KEYS) if (k in obj) { out.params.push([k, obj[k]]); consumed[k] = true; }
    } else if (format === 'anthropic') {
      if (obj.messages != null) out.messages = normAnthropicMessages(obj.messages);
      if (obj.tools != null) out.tools = normTools(obj.tools, 'anthropic');
      // 顶层 system（字符串或块数组）→ 首位 system 消息卡片
      if (obj.system != null) {
        const sysParts = typeof obj.system === 'string' ? [{ t: 'text', text: obj.system }] :
          Array.isArray(obj.system) ? obj.system.map(normAnthropicBlock) : [{ t: 'raw', label: 'system', value: obj.system }];
        out.messages = [{ role: 'system', parts: sysParts, extra: { itemType: 'system' } }, ...out.messages];
      }
      consume(STRUCT_KEYS.anthropic);
      for (const k of CC_PARAM_KEYS) if (k in obj) { out.params.push([k, obj[k]]); consumed[k] = true; }
    } else if (format === 'responses') {
      if (obj.input != null) out.messages = normResponsesInput(obj.input);
      if (obj.tools != null) out.tools = normTools(obj.tools, 'responses');
      if (obj.instructions != null) {
        const ins = typeof obj.instructions === 'string' ? [{ t: 'text', text: obj.instructions }] :
          [{ t: 'raw', label: 'instructions', value: obj.instructions }];
        out.messages = [{ role: 'system', parts: ins, extra: { itemType: 'instructions' } }, ...out.messages];
      }
      consume(STRUCT_KEYS.responses);
      for (const k of CC_PARAM_KEYS) if (k in obj) { out.params.push([k, obj[k]]); consumed[k] = true; }
    }

    for (const k of Object.keys(obj)) if (!consumed[k]) out.other[k] = obj[k];
    return out;
  }

  // ---------- 响应体：非流式解析 ----------

  function parseCCResponse(obj) {
    const out = { choices: [], usage: [], error: null, other: {}, finishReason: null };
    if (obj.error) out.error = normError(obj.error);
    if (Array.isArray(obj.choices)) {
      out.choices = obj.choices.map(ch => {
        const msg = (ch && ch.message) || {};
        const parts = [];
        if (typeof msg.content === 'string') { if (msg.content) parts.push({ t: 'text', text: msg.content }); }
        else if (Array.isArray(msg.content)) parts.push(...msg.content.map(normCCContentPart));
        else if (msg.content != null) parts.push({ t: 'raw', label: 'content', value: msg.content });
        if (typeof msg.reasoning_content === 'string' && msg.reasoning_content) {
          parts.push({ t: 'thinking', text: msg.reasoning_content, label: 'reasoning_content' });
        }
        const toolCalls = (Array.isArray(msg.tool_calls) ? msg.tool_calls : []).map(tc => {
          const fn = (tc && tc.function) || {};
          return {
            id: str(tc && tc.id), name: str(fn.name) || '(未命名)',
            args: typeof fn.arguments === 'string' ? fn.arguments : (fn.arguments != null ? JSON.stringify(fn.arguments) : ''),
          };
        });
        if (ch && ch.finish_reason) out.finishReason = ch.finish_reason;
        return { index: ch && ch.index != null ? ch.index : 0, role: str(msg.role) || 'assistant', parts, toolCalls, finishReason: ch ? ch.finish_reason : null };
      });
    }
    out.usage = normalizeUsage(obj.usage);
    for (const k of Object.keys(obj)) if (!['choices', 'usage', 'error'].includes(k)) out.other[k] = obj[k];
    return out;
  }

  function parseAnthropicResponse(obj) {
    const out = { blocks: [], stopReason: null, usage: [], error: null, other: {} };
    if (obj.error) out.error = normError(obj.error);
    out.blocks = Array.isArray(obj.content) ? obj.content.map(normAnthropicBlock) : [];
    out.stopReason = obj.stop_reason != null ? String(obj.stop_reason) : null;
    out.usage = normalizeUsage(obj.usage);
    for (const k of Object.keys(obj)) if (!['content', 'stop_reason', 'usage', 'error'].includes(k)) out.other[k] = obj[k];
    return out;
  }

  function parseResponsesResponse(obj) {
    const out = { output: [], status: null, usage: [], error: null, other: {}, outputText: null };
    if (obj.error) out.error = normError(obj.error);
    out.output = Array.isArray(obj.output) ? obj.output.map(normResponsesItem) : [];
    out.status = obj.status != null ? String(obj.status) : null;
    if (obj.incomplete_details && obj.incomplete_details.reason) out.status = 'incomplete(' + obj.incomplete_details.reason + ')';
    if (typeof obj.output_text === 'string') out.outputText = obj.output_text;
    out.usage = normalizeUsage(obj.usage);
    for (const k of Object.keys(obj)) if (!['output', 'usage', 'error', 'status', 'incomplete_details', 'output_text'].includes(k)) out.other[k] = obj[k];
    return out;
  }

  // ---------- 响应体：SSE 重组 ----------

  // CC 流重组：delta.content / delta.reasoning_content 累积，tool_calls 按 index 聚合
  // （name 取首帧、arguments 分片拼接），n>1 按 choice.index 分组，末帧 usage。
  function reassembleCC(frames) {
    const byIndex = new Map();
    let usage = null, error = null;
    const get = i => {
      if (!byIndex.has(i)) byIndex.set(i, { index: i, role: 'assistant', _text: '', _reason: '', _tc: new Map(), finishReason: null });
      return byIndex.get(i);
    };
    for (const f of frames) {
      const d = f.parsed;
      if (!d) continue;
      if (d.error) { error = normError(d.error); continue; }
      if (Array.isArray(d.choices)) {
        for (const ch of d.choices) {
          const c = get(ch && ch.index != null ? ch.index : 0);
          const delta = (ch && ch.delta) || {};
          if (typeof delta.content === 'string') c._text += delta.content;
          if (typeof delta.reasoning_content === 'string') c._reason += delta.reasoning_content;
          if (Array.isArray(delta.tool_calls)) {
            for (const tc of delta.tool_calls) {
              const idx = tc && tc.index != null ? tc.index : 0;
              if (!c._tc.has(idx)) c._tc.set(idx, { id: '', name: '', args: '' });
              const call = c._tc.get(idx);
              if (tc.id) call.id = tc.id;
              if (tc.function) {
                if (tc.function.name) call.name = tc.function.name;
                if (typeof tc.function.arguments === 'string') call.args += tc.function.arguments;
              }
            }
          }
          if (ch && ch.finish_reason) c.finishReason = ch.finish_reason;
        }
      }
      if (d.usage && (d.usage.prompt_tokens != null || d.usage.completion_tokens != null || d.usage.total_tokens != null)) usage = d.usage;
    }
    const choices = [...byIndex.values()].sort((a, b) => a.index - b.index).map(c => {
      const parts = [];
      if (c._reason) parts.push({ t: 'thinking', text: c._reason });
      if (c._text) parts.push({ t: 'text', text: c._text });
      return { index: c.index, role: c.role, parts, toolCalls: [...c._tc.values()], finishReason: c.finishReason };
    });
    return { choices, usage: normalizeUsage(usage), error, other: {} };
  }

  // Anthropic 流重组：message_start / content_block_start / *_delta / message_delta 聚合
  function reassembleAnthropic(frames) {
    const blocks = new Map();
    let usage = null, error = null, stopReason = null, model = null, id = null;
    for (const f of frames) {
      const d = f.parsed;
      if (!d) continue;
      if (d.type === 'error' && d.error) { error = normError(d.error); continue; }
      switch (d.type) {
        case 'message_start': {
          const m = d.message || {};
          id = m.id || id; model = m.model || model;
          // v1.0.0（F3）：全量拷贝 usage——此前仅取两字段会丢弃
          // cache_creation_input_tokens / cache_read_input_tokens 等缓存统计字段，
          // normalizeUsage 的未知字段兜底可将它们如实展示
          if (m.usage) usage = Object.assign({}, m.usage);
          break;
        }
        case 'content_block_start': {
          const b = d.content_block || {};
          blocks.set(d.index, {
            t: b.type === 'tool_use' ? 'toolcall' : b.type === 'thinking' ? 'thinking' : 'text',
            type: b.type, text: str(b.text), thinking: str(b.thinking),
            id: str(b.id), name: str(b.name), json: '',
          });
          break;
        }
        case 'content_block_delta': {
          const b = blocks.get(d.index);
          if (b && d.delta) {
            if (d.delta.type === 'text_delta' && typeof d.delta.text === 'string') b.text += d.delta.text;
            else if (d.delta.type === 'input_json_delta' && typeof d.delta.partial_json === 'string') b.json += d.delta.partial_json;
            else if (d.delta.type === 'thinking_delta' && typeof d.delta.thinking === 'string') b.thinking += d.delta.thinking;
          }
          break;
        }
        case 'message_delta': {
          if (d.delta && d.delta.stop_reason) stopReason = d.delta.stop_reason;
          if (d.usage && d.usage.output_tokens != null) {
            usage = Object.assign({}, usage || {}, { output_tokens: d.usage.output_tokens });
          }
          break;
        }
      }
    }
    const parts = [...blocks.entries()].sort((a, b) => a[0] - b[0]).map(([, b]) => {
      if (b.type === 'tool_use') return { t: 'toolcall', id: b.id, name: b.name, args: b.json };
      if (b.type === 'thinking') return { t: 'thinking', text: b.thinking };
      if (b.type === 'redacted_thinking') return { t: 'thinking', text: '(已加密的推理内容)', redacted: true };
      if (b.type === 'text') return { t: 'text', text: b.text };
      return { t: 'raw', label: b.type, value: { text: b.text, thinking: b.thinking } };
    });
    const other = {};
    if (id) other.id = id;
    if (model) other.model = model;
    return { blocks: parts, stopReason, usage: normalizeUsage(usage), error, other };
  }

  // Responses 流重组：优先取 response.completed / response.failed 携带的完整 output；
  // 截断（无终止事件）时退回增量累积（output_item.added + delta 拼接）。
  function reassembleResponses(frames) {
    let usage = null, status = null, error = null, completedOutput = null;
    const items = new Map(); // output_index → { item, text, args }
    const getItem = idx => {
      if (!items.has(idx)) items.set(idx, { item: null, text: '', args: '' });
      return items.get(idx);
    };
    for (const f of frames) {
      const d = f.parsed;
      if (!d) continue;
      const t = d.type || '';
      if (t === 'response.output_item.added' && d.item) {
        getItem(d.output_index != null ? d.output_index : items.size).item = d.item;
      } else if (t === 'response.output_item.done' && d.item) {
        getItem(d.output_index != null ? d.output_index : items.size).item = d.item; // 完整条目覆盖
      } else if (t === 'response.output_text.delta') {
        const it = getItem(d.output_index != null ? d.output_index : 0);
        it.text += str(d.delta);
      } else if (t === 'response.function_call_arguments.delta') {
        const it = getItem(d.output_index != null ? d.output_index : 0);
        it.args += str(d.delta);
      } else if (t === 'response.completed' || t === 'response.failed' || t === 'response.incomplete') {
        const r = d.response || {};
        if (r.usage) usage = r.usage;
        if (t === 'response.completed') status = 'completed';
        else if (t === 'response.failed') status = 'failed';
        else status = 'incomplete';
        if (r.error) error = normError(r.error);
        if (Array.isArray(r.output)) completedOutput = r.output;
      } else if (t === 'error') {
        error = normError(d);
      } else if (t === 'response.output_text.done' && d.text) {
        // 无 added 事件的开门帧兜底（个别网关），确保文本可恢复
        const it = getItem(d.output_index != null ? d.output_index : 0);
        if (!it.text) it.text = str(d.text);
      }
    }
    let output;
    if (completedOutput) {
      output = completedOutput.map(normResponsesItem);
    } else {
      output = [...items.entries()].sort((a, b) => a[0] - b[0]).map(([, e]) => {
        const item = e.item || { type: 'message', role: 'assistant' };
        if (item.type === 'message') {
          const base = normResponsesItem(item);
          if (e.text) base.parts = [{ t: 'text', text: e.text }];
          return base;
        }
        if (item.type === 'function_call') {
          return {
            role: 'assistant',
            parts: [{ t: 'toolcall', id: str(item.call_id || item.id), name: str(item.name), args: e.args || str(item.arguments) }],
            extra: { itemType: 'function_call' },
          };
        }
        return normResponsesItem(item);
      });
    }
    return { output, status, usage: normalizeUsage(usage), error, other: {} };
  }

  // ---------- 响应体解析主入口 ----------

  // 返回 { ok:true, format, isSSE, sse:{frames,bad,done,chars}, <格式特定结果> }
  // 或 { ok:false, reason, format }（reason: empty | non-json | truncated）
  function parseResponse(bodyStr, url) {
    const body = typeof bodyStr === 'string' ? bodyStr : '';
    if (!body) return { ok: false, reason: 'empty', format: detectFormat(url, '', null) };
    const truncated = isTruncated(body);
    const isSSE = looksSSE(body);
    if (isSSE) {
      const { frames, bad, done } = parseSSE(body);
      const format = detectFormat(url, '', frames);
      const stats = { frames: frames.length, bad, done, chars: body.length };
      if (format === 'cc') return Object.assign({ ok: true, format, isSSE, sse: stats }, reassembleCC(frames));
      if (format === 'anthropic') return Object.assign({ ok: true, format, isSSE, sse: stats }, reassembleAnthropic(frames));
      if (format === 'responses') return Object.assign({ ok: true, format, isSSE, sse: stats }, reassembleResponses(frames));
      // SSE 但帧格式未识别：原文回退
      return { ok: false, reason: 'unknown-format', format: 'unknown', isSSE: true, sse: stats };
    }
    const { obj, ok } = safeParse(body);
    if (!ok) return { ok: false, reason: truncated ? 'truncated' : 'non-json', format: detectFormat(url, '', null) };
    const format = detectFormat(url, body, null);
    if (format === 'unknown') {
      // 未识别格式：若有 error 字段仍解析错误卡；否则整体原文美化回退
      if (obj && typeof obj === 'object' && !Array.isArray(obj) && obj.error) {
        const other = {};
        for (const k of Object.keys(obj)) if (k !== 'error') other[k] = obj[k];
        return { ok: true, format: 'unknown', isSSE: false, error: normError(obj.error), other, usage: [] };
      }
      return { ok: false, reason: 'unknown-format', format };
    }
    let parsed;
    if (format === 'cc') parsed = parseCCResponse(obj);
    else if (format === 'anthropic') parsed = parseAnthropicResponse(obj);
    else parsed = parseResponsesResponse(obj);
    return Object.assign({ ok: true, format, isSSE: false }, parsed);
  }

  // ---------- JSON 语法高亮（原始数据标签页 / 参数 JSON 展示） ----------

  const HL_LIMIT = 200000; // 超长文本不做高亮（性能护栏）

  function esc(s) {
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  function highlightJSON(text) {
    if (text == null) return '';
    text = String(text);
    if (text.length > HL_LIMIT) return esc(text);
    const re = /("(?:\\.|[^"\\])*")(\s*:)|("(?:\\.|[^"\\])*")|-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b|\btrue\b|\bfalse\b|\bnull\b/g;
    let out = '', last = 0, m;
    while ((m = re.exec(text)) !== null) {
      out += esc(text.slice(last, m.index));
      if (m[1] !== undefined) {
        out += '<span class="j-key">' + esc(m[1]) + '</span>' + m[2];
      } else if (m[3] !== undefined) {
        out += '<span class="j-str">' + esc(m[3]) + '</span>';
      } else if (m[0] === 'true' || m[0] === 'false') {
        out += '<span class="j-bool">' + m[0] + '</span>';
      } else if (m[0] === 'null') {
        out += '<span class="j-null">' + m[0] + '</span>';
      } else {
        out += '<span class="j-num">' + esc(m[0]) + '</span>';
      }
      last = re.lastIndex;
    }
    out += esc(text.slice(last));
    return out;
  }

  // ---------- v1.0.0：调用详情数据源判定（B2 修复）----------

  // isV18Log 判定日志是否为 v1.7.5 之后的新结构（含 upstream_url / client_resp_status 字段）。
  // 旧数据（v1.7.5 之前）无这些字段：client_resp_body 列不存在、resp_body 本就是发给客户端的字节，
  // 详情页「客户端响应」需回退展示 resp_body。新数据以这些字段识别，client_resp_body 即真实客户端响应，
  // 为空表示未送达（如「客户端断开」失败日志），不应回退到 resp_body（那本是上游未送达的内容）。
  function isV18Log(l) {
    return l != null && (l.upstream_url !== undefined || l.client_resp_status !== undefined);
  }

  // clientRespBody 返回详情页「客户端响应」区块应展示的响应体：
  // 新数据取 client_resp_body（可能为空/undefined=未送达）；旧数据回退 resp_body。
  // null 安全：入参为 null/undefined 时返回 undefined（不抛错）。
  function clientRespBody(l) {
    if (l == null) return undefined;
    return isV18Log(l) ? l.client_resp_body : l.resp_body;
  }

  return {
    version: '1.0.0',
    FORMAT_LABELS,
    isTruncated,
    isV18Log,
    clientRespBody,
    parseRequest,
    parseResponse,
    highlightJSON,
    _internal: { parseSSE, looksSSE, detectFormat, normalizeUsage }, // 供调试
  };
})();
