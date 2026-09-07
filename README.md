# CPA 插件仓库

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 插件集合。当前提供 **WorkBuddy / CodeBuddy** 与支持国际版、国内版双区域的 **Qoder** 两个 OAuth Provider。

## 插件

| ID | 说明 | 源码 |
|---|---|---|
| `workbuddy` | Tencent CodeBuddy OAuth、动态模型、executor、CN 每日签到、Global 专家包、积分面板、可选积分调度 | [workbuddy/](workbuddy/) |
| `qoder` | Qoder 国际版（qoder.sh）+ 国内版（qoder.com.cn）：OAuth / PAT、动态模型、流式推理、CN 每日签到、积分面板、token 保活 | [qoderwork/](qoderwork/) |

## 多架构 Release

每个插件独立版本发 Release（tag `<id>-v*`），产物为 CPA 插件商店标准格式：

```text
<id>_<version>_linux_amd64.zip      # zip 根目录: <id>.so
<id>_<version>_linux_arm64.zip
<id>_<version>_darwin_amd64.zip     # <id>.dylib
<id>_<version>_darwin_arm64.zip
<id>_<version>_windows_amd64.zip    # <id>.dll
<id>_<version>_windows_arm64.zip
<id>_<version>_freebsd_amd64.zip
checksums.txt
```

命名规则与官方一致：`ArchiveName(id, version, goos, goarch) = {id}_{version}_{goos}_{goarch}.zip`
（见 CLIProxyAPI `internal/pluginstore`）。

CI：push / PR 全量构建（只出 artifacts）；tag `<id>-v*`（如 `qoder-v0.1.0`）或 dispatch 触发**该插件独立版本**的 Release。

## 安装（linux/amd64 示例）

```bash
# 从 Release 下载
unzip qoder_0.1.0_linux_amd64.zip
# 扁平 plugins 目录（常见 docker 挂载）
cp qoder.so /path/to/cliproxyapi/plugins/qoder.so
# 或平台子目录布局
# mkdir -p plugins/linux/amd64 && cp qoder.so plugins/linux/amd64/
```

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy:
      enabled: true
    qoder:
      enabled: true
```

## 远程更新（插件商店自定义源）

CPA 插件商店源添加：

```text
https://raw.githubusercontent.com/SadPull/cpa-plugin/main/registry.json
```

然后在商店 UI 安装/更新 **workbuddy** 和 **qoder**。
