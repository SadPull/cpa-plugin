# cpa-model-visibility

CLIProxyAPI 插件：按下游 API Key 过滤 `/v1/models` 模型列表，让每个 key 只"看到"自己有权限的模型。

请求层的拦截（能不能用）由 key-billing 等机制负责；本插件只负责**可见性**（列表里显示什么）。两层互补：列表里被隐藏的模型请求不了，列表里可见的模型仍可能被请求层拦截。

## 工作原理

声明 `response_interceptor` 能力，实现 `response.intercept_after` 钩子。CPA v8 的所有模型列表响应（OpenAI `/v1/models`、Claude、Grok、Home 分支）都经过 `WriteModelListResponse`，会把完整列表 JSON + 客户端请求头交给本插件；插件按调用方 key 规则过滤 `data` 数组后返回新 body。

- 该钩子对所有非流式响应都会触发，插件严格按响应体形状识别模型列表（`{"data":[...],"object":"list"}`，元素含 `id`/`object:"model"`），其余响应一律原样透传。
- 调用方身份从 `Authorization: Bearer` / `X-Api-Key` / `X-Goog-Api-Key` 请求头还原。
- 解析失败一律 fail-open（透传原始响应）：多显示一个模型无害，弄坏响应有害。

## 配置

```yaml
plugins:
  configs:
    cpa-model-visibility:
      enabled: true
      debug: false        # 记录每次过滤（key 掩码、隐藏数量）
      default: empty      # 无规则 key 的默认可见性：empty（空列表）| full（全部）
      rules:
        - keys: ["sk-fsht12345"]
          deny: ["gpt-*", "codex-*"]     # 隐藏 codex 系模型
        - keys: ["sk-xxw-xxwnb"]         # 空 allow/deny = 全部可见
        - keys: ["sk-某受限key"]
          allow: ["glm-*", "kimi-*"]     # 白名单模式
```

语义：

- **模式匹配**（大小写不敏感，同 oauth-excluded-models 方言）：精确匹配；`abc*` 前缀；`*abc` 后缀；`a*bc` 两端锚定；`*` 全部。**不含 `*` 的模式就是精确匹配**（不会子串误伤）。只支持一个 `*`。
- **维度**：`allow`/`deny` 按模型 id 匹配；`allow_owned_by`/`deny_owned_by` 按条目 `owned_by`（如 openai、workbuddy、antigravity）匹配。
- **规则**：visible =（allow 非空时命中 allow）∧（owned_by allow 同理）∧ ¬deny ∧ ¬deny_owned_by。deny 永远赢。
- 一个 key 只能出现在一条规则里（重复即配置报错）；无规则 key 走 `default`。
- 规则改动改 config.yaml 即可，CPA 会热重载（`plugin.reconfigure`），无需重启。

## 管理接口（只读）

```text
GET /v0/management/plugins/cpa-model-visibility/rules   # 当前生效规则
GET /v0/management/plugins/cpa-model-visibility/check?key=sk-xxx   # 某个 key 命中的规则
```

某 key 实际看到的列表，直接带该 key 请求 `/v1/models` 即可。

## 构建

```sh
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -tags cshared \
  -ldflags "-s -w -X cpa-model-visibility/internal/plugin.version=0.1.0" \
  -o dist/cpa-model-visibility.so ./cmd/cpa-model-visibility
```

产物放到 CPA 的 `plugins/linux/amd64/`（或扁平 `plugins/`），重启 CPA 生效。

## 环境要求

CLIProxyAPI v8.0.0+（`WriteModelListResponse` 响应拦截钩子自该系列引入；本插件按 v8.0.11 的 RPC 契约实现，schema_version 6）。宿主需 CGO 构建（支持插件）。
