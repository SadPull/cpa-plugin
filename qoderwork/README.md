# Qoder CPA 插件

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Qoder Provider 插件：**国际版（qoder.sh）+ 国内版（qoder.com.cn）双区域兼容**，多账号 OAuth 设备授权 / PAT 双登录、动态模型、每日签到（CN）、积分面板、token 自动保活。

国际版推理走 Qoder CLI 现行 OpenAI 兼容端点（`api2-v2.qoder.sh/model/v1/chat/completions`，纯 Bearer，原生透传 messages/tools），国内版走 COSY 签名网关（`gateway.qoder.com.cn`），按账号区域自动路由。

> 本插件是 [QoderGateway](https://github.com/SadPull/QoderGateway)（Python 版 Qoder 反代）的插件化形态，协议逆向成果与 [cpa-plugin](https://github.com/Sliverkiss/cpa-plugin)（qoderwork 插件）一脉相承。

## 功能

| 能力 | 说明 |
|---|---|
| **双区域** | 每个账号独立区域（`global` / `cn`）：国际版 OpenAI 原生 SSE 直通；国内版 COSY 签名 + QoderEncoding + 嵌套 SSE 解析 |
| **双登录方式** | ① OAuth 设备授权（PKCE，浏览器授权，dt- 30 天 + drt- 1 年自动旋转）② PAT 导入（pt-，长期有效兜底）——两家族可共存于同一 auth 文件 |
| **OpenAI 原生透传**（国际版） | messages（多模态内容）、tools、tool_calls、temperature、max_tokens、reasoning_effort 等字段全量透传上游 |
| **动态模型** | 上游模型列表动态拉取（CN 用 COSY 签名），静态 11 模型兜底（auto/lite + Qwen/DeepSeek/GLM/Kimi/MiniMax 系列） |
| **每日签到**（CN） | 面板手动签到（单账号/批量）+ 09:00/21:00 定时自动签到，签到后返回最新积分快照 |
| **积分面板** | 账号卡片：昵称/区域/积分/计划/签到状态/操作（签到/刷新/选用），国际版显示 quota |
| **token 保活** | 22:00 定时刷新；按 token 前缀路由（drt- → deviceToken/refresh，jrt- → jobToken/refresh），旋转式刷新自动回写，PAT 永不劫持 OAuth 刷新 |
| **生命周期** | 额度耗尽自动禁用（不删号），签到恢复积分后自动重新启用 |
| **auth 隔离** | 文件名前缀 `qoder-` 过滤 + 内容域名校验，与 workbuddy 等其他插件互不干扰 |

## 安装

### 从 Release（推荐）

```bash
# 按你的平台下载（示例 windows/amd64）
unzip qoder_0.1.0_windows_amd64.zip
cp qoder.dll /path/to/cliproxyapi/plugins/qoder.dll
```

支持平台：linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64（另附 freebsd/amd64、windows/arm64 交叉产物）。

### 从源码

```bash
cd cpa-plugin/qoder
CGO_ENABLED=1 go build -buildmode=c-shared -ldflags "-X main.version=0.1.0" -o qoder.so .
cp qoder.so /path/to/cliproxyapi/plugins/
```

> Windows 需要 MinGW-w64 gcc，Linux/macOS 需要 cc/gcc。

### config.yaml

```yaml
plugins:
  enabled: true
  dir: ./plugins
  configs:
    qoder:
      default_region: global      # 新登录/导入的默认区域：global | cn
      checkin_auto: true          # CN 账号每日自动签到
      lifecycle_auto: true        # 额度耗尽自动禁用
      token_keepalive: true       # 每日 22:00 token 保活
      models_refresh: true        # 后台定时拉取上游模型目录（新模型自动跟进）
      models_refresh_minutes: 10  # 拉取间隔（分钟，最小 1）
      models_refresh_push: true   # 目录变化时改写账号文件触发 CPA 立即重注册
      # scheduler_mode: credits   # 可选：积分感知调度
      # client_id: <uuid>         # 可选：覆盖设备授权 client_id
```

### 模型列表自动更新

上游 Qoder 会不定期上新/下线模型，而 CPA 只在启动、config 重载或账号文件变动时才向插件查询模型列表。qoder 插件因此内置三层机制，无需重启即可跟进最新目录：

1. **后台定时拉取**（`models_refresh` / `models_refresh_minutes`）：每个区域每次任选一个可用账号请求一次模型目录，失败保留上一次成功结果，绝不回退到静态兜底覆盖注册表。
2. **变化推送**（`models_refresh_push`）：检测到模型增减时，插件将该区域所有启用中的账号文件加一个 `models_synced_at` 时间戳写回（`host.auth.save`），CPA 的文件监控随之重查并重新注册模型 —— CPA 没有插件→host 的推送 RPC，这是唯一可靠的触发通道。目录无变化时零写入。
3. **请求兜底**：推理请求遇到目录里不认识的模型时，插件自动触发一次节流的后台刷新（当次请求仍按透传兜底处理）。

模型解析顺序：动态目录（`slug(display_name)` 或原始 key）→ 内置翻译表 → 原样透传（上游未知 key 静默路由到 auto）。

## 登录 / 导入

1. **OAuth 设备授权（推荐）**：CPA 管理界面 → 该 provider 登录入口 → 浏览器打开授权页 → 确认后自动落盘。面板登录弹窗支持选择区域；插件 `/login` 的 metadata 也可带 `{"region":"global"|"cn"}`。
2. **PAT 导入**：面板 →「导入 Qoder 凭证」→ 选区域 → 粘贴 `pt-` 开头的 PAT。PAT 创建页：
   - 国际版：`qoder.sh → 设置 → Personal Access Tokens`
   - 国内版：`qoder.com.cn → 设置 → Personal Access Tokens`
3. **直接导入 dt-/drt-**：将 `{"auth":{"accessToken":"dt-...","refreshToken":"drt-...","region":"global"},"account":{"uid":"..."}}` 保存为 `qoder-<uid>.json` 放入 CPA auths 目录（与 [qodergate-register](../../qodergate-register) 注册机批量导出格式兼容）。

## 双区域协议对照

| | 国际版 global | 国内版 cn |
|---|---|---|
| 推理端点 | `api2-v2.qoder.sh/model/v1/chat/completions` | `gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation` |
| 认证 | 纯 `Bearer dt-/jt-` | COSY 签名（RSA 包 AES + MD5） |
| 请求体 | OpenAI 原生（透传） | baseprompt 模板 + QoderEncoding 编码 |
| 响应 | 标准 OpenAI SSE | 嵌套 SSE（外层 `body` 字段二次解析） |
| 模型列表 | 静态兜底（动态端点探测中） | `GET /algo/api/v2/model/list?Encode=1`（COSY 签名） |
| 签到/积分包 | 无（仅 quota） | `sash/api/v1/me/daily-check-in/*` |
| 设备授权 | `qoder.com/device/selectAccounts` + `openapi.qoder.sh` poll | `qoder.com.cn/device/selectAccounts` + `openapi.qoder.com.cn` poll |

## 真机验证（可选）

```bash
QODER_LIVE_TOKEN=dt-xxxx go test -tags live -v -run TestLiveGlobal ./...
```

依次验证 userinfo → chat 流式 → quota 全链路（只读，不会刷新/旋转 token）。

## 面板

CPA 管理界面 → 插件 → Qoder 面板：区域筛选、积分汇总、签到、选用路由账号、PAT 导入（带区域选择）。

## 致谢

- [Sliverkiss/cpa-plugin](https://github.com/Sliverkiss/cpa-plugin) — workbuddy/qoderwork 插件骨架与本插件的 RPC 层蓝本
- [bzym2/QoderGateway](https://github.com/bzym2/QoderGateway) / [cubk1/qoder2api](https://github.com/cubk1/qoder2api) — Qoder 协议逆向成果

## License

MIT
