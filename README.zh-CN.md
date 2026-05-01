# gamehub-for-mac-api-stub

作者 **BioHaz616**。基于 [MIT License](./LICENSE) 发布。

> **本项目与 GameHub 厂商无任何关联。** 本项目是独立第三方兼容性工具,
> 不修改 GameHub 应用本身或其附带的任何文件。完整声明请参见文末
> "免责声明与法律说明" 章节。

[English README](./README.md)

## 项目目的

macOS 应用 **GameHub**(由 Guangzhou Chicken Run Network Technology
Co., Ltd. 发行的基于 Wine 的 Windows 游戏启动器)在进入主界面前,
要求用户通过 `api-international-gamehub.xiaoji.com` 完成邮箱验证码
登录。厂商的真实后端会拒绝白名单之外的邮箱,返回
*"No internal test qualification yet"*(尚无内部测试资格)
错误,因此没有预先获批账号的用户根本无法通过登录界面。

本项目是一个本地 TLS 服务器,使用户能够在 macOS 上使用 GameHub
**而无需提供有效的登录凭据**。它在认证流程中冒充 xiaoji 后端,
接受任意邮箱与任意验证码,并向 GameHub 返回一个合成的用户对象,
使应用进入"已登录"状态。公共内容(游戏目录、语言列表、商店列表)
仍由真实上游提供,以保证界面正常显示;包含识别信息的崩溃报告与
遥测请求则在本地应答,确保数据不外泄。

Steam 与 Epic 的认证不受影响,它们由内置客户端直接与各自的厂商通信。

## 为什么需要本工具

GameHub 的 UI 基于 Tauri 2 webview。Vue 前端在构建时已将 API 基础
URL 写死到打包后的 JavaScript 中(`VITE_API_BASE_URL`),
因此运行时环境变量 `GAMEHUB_API_BASE_URL` 只能改变 Rust 端的请求
地址,webview 中的 `fetch()` 调用仍然访问原始域名。要重定向这些流量
就需要:

1. 在 `/etc/hosts` 中将该域名指向 `127.0.0.1`;
2. 一个 TLS 服务器,提供系统信任库认可的证书。

本程序同时完成上述两点,并提供 API 响应。

## 运行模式

启动时可选两种模式:

- **直通模式(默认)。** 仅拦截认证相关接口(登录、验证码、用户信息、
  退出登录)以及若干会发送识别信息的遥测接口。其余请求转发至真实
  上游,因此字典、语言、横幅、商店列表等公共内容会以真实数据返回,
  应用界面可以完整使用。
- **纯 stub 模式(`-stub-only`)。** 不向上游转发任何请求,所有未
  显式处理的路径都返回通用成功响应
  `{"code":200,"message":"success","data":{}}`。适合需要完全断网、
  不向厂商发送任何流量的场景。代价是依赖真实内容的界面(首页横幅、
  搜索、商店)会显示为空。

除非传入 `-strip-pii=false`,否则所有转发请求都会自动剥离识别性
请求头(`X-Device-Id`、各种 forwarded-for 变体等)。

## 环境要求

- macOS(已在 Apple Silicon 上验证)
- Go 1.22 或更新版本
- 一次性安装 [mitmproxy](https://mitmproxy.org/) 用于获取其 CA 证书。
  你也可以使用自己控制的任意 CA,mitmproxy 只是最方便的来源。
- mitmproxy CA 已被 macOS 系统钥匙串信任(一次性配置,详见下文)。
- `sudo` 权限(用于绑定 443 端口及修改 `/etc/hosts`)。

## 一次性配置

### 1. 生成并信任 CA

最简单的方式是使用 mitmproxy 自动生成的 CA,本程序会直接读取它:

```sh
brew install mitmproxy
mitmproxy   # 按 q、y 退出,这会生成 ~/.mitmproxy/* 目录
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain \
  ~/.mitmproxy/mitmproxy-ca-cert.pem
```

验证:

```sh
security find-certificate -c mitmproxy /Library/Keychains/System.keychain >/dev/null && echo OK
```

如需使用自己的 CA,运行时通过 `-ca=/path/to/ca.pem` 指定路径,
PEM 文件中必须同时包含证书与私钥。

### 2. 编译

```sh
git clone <this-repo>
cd gamehub-for-mac-api-stub
go build -o gamehub-for-mac-api-stub .
```

## 运行

```sh
sudo ./gamehub-for-mac-api-stub
```

启动流程:

1. 加载 CA(默认 `~/.mitmproxy/mitmproxy-ca.pem`)。
2. 在 `/etc/hosts` 中加入两条记录:
   ```
   127.0.0.1 api-international-gamehub.xiaoji.com
   127.0.0.1 api-cn-gamehub.xiaoji.com
   ```
   这些记录被包裹在带明显注释的代码块中,程序退出时自动清理。
3. 在 `:443` 端口启用 TLS 监听,根据 SNI 动态签发服务器证书
   (使用上一步加载的 CA 签名)。
4. 通过 `sudo -u $SUDO_USER` 以调用者身份启动
   `/Applications/GameHub.app`,以便其写入正确的用户家目录。
5. 将所有非认证、非遥测的请求转发到真实后端。

GameHub 登录界面出现后:

1. 输入任意邮箱地址。
2. 点击 **Send Code**(发送验证码)。
3. 输入任意 6 位数字。
4. 点击 **Login**(登录)。

按 **Ctrl+C** 退出。`/etc/hosts` 条目会在退出前自动恢复。

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-listen` | `:443` | 绑定地址。修改时需同步调整重定向目标。 |
| `-ca` | `~/.mitmproxy/mitmproxy-ca.pem` | 用于签发 SNI 证书的 CA 证书 + 私钥(PEM 格式)。 |
| `-app` | `/Applications/GameHub.app/Contents/MacOS/GameHub` | GameHub 可执行文件路径。 |
| `-launch` | `true` | 启动时自动拉起 GameHub。 |
| `-no-hosts` | `false` | 跳过 `/etc/hosts` 修改(此时需自行用其他方式重定向)。 |
| `-stub-only` | `false` | 永不转发到上游;未处理路径返回通用成功响应。 |
| `-upstream` | `https://api-international-gamehub.xiaoji.com` | 直通模式的转发目标。 |
| `-strip-pii` | `true` | 转发请求时剥离识别性请求头(X-Device-Id 等)。 |
| `-http` | `false` | 在 `:8443` 上以纯 HTTP 监听(不启用 TLS、不修改 hosts、无需 root)。仅用于本地测试,因为真实 URL 是 HTTPS,无法作为重定向目标。 |
| `-v` | `true` | 详细记录请求/响应日志。 |

## 已 stub 的接口

无论运行在哪种模式,以下接口都由本地应答:

| 方法 | 路径 | 拦截原因 |
|------|------|----------|
| `POST` | `/oauth/v1/public/verify-code/send` | 真实后端拒绝不在白名单内的邮箱。 |
| `POST` | `/oauth/v1/public/login` | 同上,真实上游会返回 400 "verification code invalid"。 |
| `POST` | `/oauth/v1/public/logout`、`/oauth/v1/user/logout` | 合成的 token 无效,真实退出会 401。 |
| `GET`  | `/oauth/v1/user/{profile,info,me,current,account}`、`/game/v1/user/{profile,info,me}` | 携带合成 bearer 时真实接口会 401。显式列出避免通配 `/user/`(搜索、库等)误匹配,保留它们的转发行为。 |
| `POST` | `/game/v1/public/report/sync`、`/game/v1/public/report/...` | 遥测,留在本地。 |
| `POST` | `/game/v1/user/crash-feedbacks/check`、`/game/v1/user/crash-feedbacks` | 会以 `crash_id` 字段发送主机 MAC 地址,留在本地。 |
| `GET`  | `/oauth/v1/user/third-party/bindings`、`/oauth/v1/user/bindings` | 前端会对该列表调用 `.filter()`,必须返回数组。无关联第三方账号时返回 `[]`。 |
| `GET`  | `/game/v1/user/game_lib/list` | 返回空分页结构。合成 token 无法获取真实库数据,详见"已知限制"。 |
| `GET`  | `/game/v1/user/simulator/component` | 返回空分页结构。Steam 启动所需组件元数据需要真实账号,详见"已知限制"。 |
| `OPTIONS *` | `*` | `204` 配合宽松的 CORS 头(与真实后端一致)。 |

其余请求在默认模式下被转发至上游,且转发前已剥离识别性请求头。

每个 stub 使用的响应外层结构为:

```json
{ "code": 200, "message": "...", "data": ... }
```

`code: 200` 表示成功;真实后端不使用 `code: 0`。

## 调试与扩展

如果某个具体功能异常(应用某屏报错、某个功能不可用),应关注直通
代理打印的上游状态码:

```
[passthrough] 401 api-international-gamehub.xiaoji.com/some/path  (auth-rejected, candidate to stub)
```

`401` 或 `403` 意味着真实后端拒绝了我们的合成 token,这正是需要新增
处理器的路径。在 `main.go` 中加入:

```go
mux.HandleFunc("GET /game/v1/some/new/path", func(w http.ResponseWriter, r *http.Request) {
    ok(w, r, map[string]any{ /* 与 UI 期望相符的结构 */ })
})
```

要确认真实后端在该路径上返回的具体结构(以便 stub 模拟其形状),
可以使用 mitmproxy 等工具抓取一次,或临时去掉 stub 让请求落到直通
模式(此时需要使用真实账号登录)。

## 架构

```
+-----------------+     +------------------+     +---------------------+
|  GameHub.app    |     |  /etc/hosts      |     |  gamehub-for-mac-api-stub       |
|  (Tauri + Vue)  |fetch| routes xiaoji    |TLS  |  TLS terminator     |
|                 |---->| to 127.0.0.1     |---->|  + handlers         |
+-----------------+     +------------------+     +----------+----------+
                                                            |
                                       +--------------------+--------------------+
                                       |                    |                    |
                                auth + telemetry      catch-all (default)    -stub-only
                                (always stubbed)            |                    |
                                                strip PII headers          generic success
                                                            |
                                                  +---------v---------+
                                                  |  real upstream    |
                                                  |  (xiaoji.com)     |
                                                  +-------------------+
```

TLS 终端使用 `crypto/tls` 的 `GetCertificate` 回调,每次 ClientHello
触发一次。它会在内存中生成一张 ECDSA P-256 服务器证书,用加载的 CA
签名,并将所请求的 SNI 同时写入 Subject CN 与 SubjectAltName,然后
缓存。因此同一个服务器无需配置即可同时服务 `*.xiaoji.com` 以及
任何被加入 `/etc/hosts` 的域名。

## 已知限制

- **启动 Steam 游戏需要厂商专有组件,这些组件由真实账号才能下载。**
  首次尝试启动 Steam 游戏时,GameHub 会通过
  `GET /game/v1/user/simulator/component` 获取两个组件的元数据:
  `steamAgent`(type 7)与 `steamClient`(type 8)。该接口需要真实
  认证会话,合成 token 会被拒绝,且没有任何无认证的备用路径。
  组件下载 URL 也未内置在应用二进制中,任何无认证接口都无法获取。

  实用绕过:用一个真实账号登录一次,让 GameHub 把这两个组件下载
  到 `~/Library/Application Support/com.gamemac.www/wine-engine/`,
  之后再切回本工具运行。前端的 `[steam-deps]` 检查会在调用 API
  之前先查本地缓存,组件落盘后就不再依赖该接口。

  替代方案:不使用 GameHub,而是改用 Whisky、CrossOver 或 Apple
  Game Porting Toolkit 直接运行 Steam,这些方案没有任何厂商侧的
  元数据门槛。

- **Steam 与 Epic 账号认证不在本工具范围内。** GameHub 通过内置的
  steamkit-core 直接与 Valve 通信,通过 OAuth 与 Epic 通信,这些
  流量不经过 xiaoji API,本工具完全不影响。要离线游玩 Steam 游戏,
  仍需先在 GameHub 内的 Steam 客户端登录一次,然后启用 Steam 自带
  的离线模式。

- **默认模式仍会与厂商通信。** 公共内容接口(字典、横幅、商店列表)
  会被转发,以保证界面正常工作。本工具会剥离识别性请求头,但上游
  仍可看到你的 IP、User-Agent 与请求形态。如需完全网络隔离,
  请使用 `-stub-only`,接受部分界面会显示为空。

- **token 绑定字段疑似仅在客户端使用。** 应用中能找到
  `userTokenHash`、`deviceFingerprint`、`localBindingKeyHash`、
  `localBindingSignature` 等字段,但静态分析显示它们由本地推导
  而非由服务器签发并校验。如果出现绑定相关错误,该假设便不再
  成立。

- **Tauri 自动更新模块写死了中国大陆端点**
  (`api-cn-gamehub.xiaoji.com`),所以它一定会命中本工具配置的
  重定向。本工具对该接口返回 `204`(Tauri 更新器约定的"无更新"
  响应)。

- **不影响 SIP。** 本工具不需要关闭 SIP(System Integrity
  Protection)。它对运行环境的修改(`/etc/hosts`、443 端口)
  属于普通管理员权限,不属于 SIP 保护范围。

## 清理

如果本工具被强行终止(`kill -9`、崩溃等)而未走完正常退出流程,
`/etc/hosts` 中的条目会残留。手动恢复:

```sh
sudo sed -i '' '/# gamehub-for-mac-api-stub managed entries/,/^$/d' /etc/hosts
```

或手动删除该代码块,它由明显的注释包裹。

## 免责声明与法律说明

### 无关联声明

本项目**与** Guangzhou Chicken Run Network Technology Co., Ltd.、
GameHub、xiaoji.com 及其任何子公司、员工、代表**无任何关联,亦
未受其授权、赞助或认可**。*GameHub* 及任何相关商标均为各自所有
人的财产,在本文中仅用于标识本工具所对接的应用。

### 本工具做了什么、未做什么

本工具**不会**:

- 修改、打补丁、重新编译或以其他方式改动 GameHub 应用二进制或其
  附带的任何文件;
- 解密、提取、传播或转载厂商专有的代码、资产或内容;
- 绕过用于保护付费内容的数字版权管理(DRM);
- 让用户访问其本人尚未拥有、尚未在本机安装的游戏或内容;
- 绕过任何支付或授权系统。

本工具**会**:

- 作为独立进程在用户自己的设备上运行;
- 通过修改用户自己的 `/etc/hosts` 文件来拦截发往厂商服务器的网络
  流量(`/etc/hosts` 是用户对自己系统拥有完全修改权限的配置文件);
- 提供一个由用户自己控制并主动信任的 CA 所签发证书的备选 TLS
  端点;
- 用合成数据应答请求,使应用表现得像存在远端后端一样。

游戏内容、Steam 认证、Epic 认证均不在本工具的拦截范围内,所有相关
流量均原样直达各自厂商。

### 互操作性定位

本工具用于**互操作性与个人兼容性目的**:让已合法持有 GameHub
应用副本的用户能够在自己的硬件上使用该应用,而不必受制于其可能
不接受其账号的第三方认证系统。

在具备相关法律条款的法域(例如美国 17 U.S.C. § 1201(f),或欧盟
指令中类似的互操作性条款)中,此类兼容层一般不被视为"规避技术
保护措施"。**以上内容不构成法律意见,如有疑问请咨询本地律师。**

### 服务条款与最终用户许可协议

使用本工具可能违反 GameHub 的服务条款、最终用户许可协议
(EULA)或可接受使用政策。**用户需自行审阅并遵守上述条款。**
违反合约与计算机滥用法律是两个独立的问题,前者属于用户与厂商
之间的事项。

### 无担保

本软件按"原样"提供,不附带任何明示或默示担保,包括但不限于
适销性、特定用途适用性以及不侵权的担保。作者及贡献者对因使用
本软件而产生的任何损害不承担责任,包括但不限于数据丢失、账号
封禁,以及任何附带或后果性损害。

### 负责任地使用

本项目仅出于**教育、研究及个人兼容性使用**的目的发布。请勿用于
盗版、账号共享、滥用厂商基础设施或任何违法活动。如果直通模式
产生的持续流量会对厂商基础设施造成可见影响,请切换至 `-stub-only`
模式。
