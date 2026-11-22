# Kiln HTTP 上传协议 v1（`release.uploads`）

发布 job 可把 release 文件（与 GitHub Release 同一批 asset 和 `SHA256SUMS`）发到任何实现本协议的 HTTPS 服务（如官网下载页）。本文写给**服务端实现者**；设计背景见 [PLAN #13.9](PLAN.md)（路由到 Outline `06 [K-GH-REL]`），参考实现 `internal/fakeupload`（内存版），客户端 `internal/upload`。

## workflow 写法

```yaml
publish:
  needs: [dmg]
  environment: release                 # 凭据在它的 secret 中
  release:
    assets:                            # github、uploads 共用
      - from: dmg.dmg
        name: myapp-${{ github.ref_name }}-arm64
        archive: none
    github: {}                         # 可选
    uploads:
      - name: myapp-site               # 可选
        url: https://<官网域名>/api/releases
        token: ${{ secrets.RELEASE_TOKEN }}
        latest: auto                   # auto（默认）| true | false
        prerelease: auto               # auto（默认）| true | false
```

| 字段 | 说明 |
|---|---|
| `url` | 必填，即下文的 `{url}`。必须 `https://`（主机为 `localhost`、`127.0.0.0/8`、`::1` 时可用 `http://`，供本地测试）；不能带查询参数、fragment 或用户信息。可引用 `vars`、`github`，规划时求值 |
| `token` | 必填，必须引用 secret（如 `${{ secrets.X }}`），只在发布阶段由 Server 解密，构建 job 拿不到 |
| `name` | 展示名（Dashboard、日志），默认 `url` 的主机名 |
| `latest` | `auto`：本版本是正式版（tag 去掉开头 `v` 后是无预发布段的 semver）且不低于 Kiln 向同一 `url` 发布过的所有正式版时为 true；`true` / `false`：固定 |
| `prerelease` | `auto`：tag 是带预发布段的 semver（如 `v1.2.0-rc.1`）时为 true；`true` / `false`：固定 |

## 公共约定

- 每个请求带 `Authorization: Bearer <token>` 和 `User-Agent: kiln/<版本>`。token 由服务端签发，建议只有上传安装包的权限。
- `{tag}` 是 tag 名原样（如 `v1.0.0`），`{name}` 是 asset 文件名（如 `myapp-v1.0.0-arm64.dmg`、`SHA256SUMS`），都按 URL **path segment** 转义（`/` → `%2F`，空格 → `%20`）。接受哪些字符由服务端决定，不接受时返回 `400 bad_name`；服务端要防止 `..`、`/` 等路径穿越。
- 成功响应体是 JSON（`Content-Type: application/json`），`PUT` 的成功响应可以没有响应体。
- 错误响应体是 JSON：`{"error": "<错误码>", "message": "<说明>"}`。Kiln 在日志和 Dashboard 中显示方法、路径、状态码、错误码和说明（截断，token 换成 `***`），不显示请求头。
- **重试**：网络错误、`429`、`5xx` 按指数退避重试（连同第一次最多 5 次，1 秒起翻倍，最长 30 秒；`429` / `5xx` 带 `Retry-After: <秒数>` 且不超过 60 秒时按它等待，更长或写成 HTTP 日期时按普通退避）；其他 `4xx` 不重试，发布 job 直接失败。因此所有请求都必须**幂等**。
- **不跟随重定向**：`3xx` 视为错误（token 和文件只发给配置的地址），`url` 要写最终地址。
- tag **已发布**（published）后不可变：不同的内容 `409 published`，完全相同的重试必须 `200`。
- `sha256` 均为小写 hex。

## 端点

查询 → 暂存（每个文件一次，含 `SHA256SUMS`）→ 提交；认证失败返回 `401` / `403`。

### 查询 `GET {url}/{tag}`

`200` 返回 `{"tag", "state": "staging" | "published", "assets": [{"name", "size", "sha256"}], "published_at"?}`；没有这个 tag 时 `404`。用于续传（已暂存且一致的文件不再上传）和重跑（已以相同内容发布的 tag 视为完成）。

- `state: staging`：`assets` 是已暂存的文件。
- `state: published`：`assets` 是发布时的清单，带 `published_at`（RFC 3339）。Kiln 比较它与本次文件（名称、大小、sha256 的集合）：相同 → 视为已发布，跳过；不同 → 发布 job 失败（"已经发布过不同的内容"），不覆盖。
- 不支持查询时可返回 `405` 或 `501`，Kiln 会上传全部文件（依赖 PUT、POST 的幂等性），但建议实现。

### 暂存 `PUT {url}/{tag}/assets/{name}`

```http
PUT /api/releases/v1.0.0/assets/myapp-v1.0.0-arm64.dmg HTTP/1.1
Authorization: Bearer <token>
User-Agent: kiln/v0.1.20
Content-Type: application/octet-stream
Content-Length: 73400320
Content-Digest: sha-256=:n4bQgYhMfWWaL+qgxVrQFaO/TxsrC4Is0V1sFbDwCgg=:

<文件字节>
```

请求带 `Content-Length` 和 `Content-Digest: sha-256=:<base64>:`（[RFC 9530](https://www.rfc-editor.org/rfc/rfc9530)），请求体为文件原始字节。服务端必须：

1. 校验 `Content-Digest` 的 sha-256 与内容一致、长度与 `Content-Length` 一致，否则 `400 digest_mismatch` 并丢弃内容。
2. publish 之前文件**对外不可见**（不出现在下载页，下载地址取不到）。
3. 同名文件已有**完全相同**的内容 → `200`（不论 tag 是否已发布）；tag 已发布且内容不同（或不是已发布的文件）→ `409 published`；仍在 staging 的新文件或内容不同的文件 → 保存（覆盖），`201`。Kiln 只把 `200`、`201` 当作成功。
4. 超过大小限制 → `413 too_large`（Kiln 不重试）。

### 提交 `POST {url}/{tag}/publish`

```json
{
  "commit": "0123456789abcdef0123456789abcdef01234567",
  "run_id": 1234,
  "latest": true,
  "prerelease": false,
  "assets": [
    {"name": "SHA256SUMS", "size": 102, "sha256": "…"},
    {"name": "myapp-v1.0.0-arm64.dmg", "size": 73400320, "sha256": "9f86d0…"}
  ]
}
```

服务端必须：

1. 核对清单中每个文件都已暂存且大小、sha256 一致；有一个不符就 `409 incomplete`，什么都不发布。
2. **原子地**对外发布清单中的文件；清单之外的暂存文件丢弃。
3. `latest: true` 时把"最新版"切到这个 tag，`false` 时不改变；`prerelease` 供下载页标注。
4. 已发布过：清单相同 → 返回与第一次相同的 `200` 响应，不做任何改变（包括"最新版"）；清单不同 → `409 published`。
5. `commit`、`run_id` 只供服务端记录与排查。

成功响应 `200` 如下，各文件的 `url` 是下载地址（可选），Kiln 在 Dashboard 的发布详情中列出。

```json
{
  "tag": "v1.0.0",
  "published_at": "2026-11-02T12:00:00Z",
  "assets": [
    {"name": "myapp-v1.0.0-arm64.dmg", "size": 73400320, "sha256": "9f86d0…", "url": "https://<官网域名>/download/v1.0.0/myapp-v1.0.0-arm64.dmg"}
  ]
}
```

## Kiln 的调用顺序

发布 job 可同时有 GitHub Release、多个上传目标和镜像。Kiln 保证：

1. **阶段 0**：Server 把 `release.assets` 打包一次并生成 `SHA256SUMS`，各文件类目标发布同一份字节。
2. **阶段 A（暂存）**：确认 tag 仍指向本次构建的 commit，解密凭据；npm、镜像先检查和暂存；GitHub Release 建 draft 并上传；每个上传目标 `GET` 查询后对缺少或不一致的文件逐个 `PUT`。任何目标在此阶段失败，**所有目标都不提交**（GitHub 停在 draft，上传目标停在 staging），发布 job 失败。
3. **阶段 B（提交）**：发布 npm 包 → 设置镜像 tag → 依次 `POST publish` 各上传目标 → 最后正式发布 GitHub Release。某步失败时已提交的保持已提交；重跑时已提交的目标在阶段 A 的查询中被识别（`state: published` 且清单相同）并跳过，只补剩下的。

## 安全

- 只用 HTTPS、不跟随重定向；token 只发给 `url` 所在服务，日志中替换为 `***`。
- token 存在 environment secret 中，只有 ref 策略允许的 tag（如 `refs/tags/v*`）触发的发布 job 能解密；解密写审计日志 `publish.credentials`，每次提交写 `publish.upload`。
- Kiln 不限制 `url` 指向的地址（不做内网地址拦截）：只构建白名单账号的自有仓库（PLAN #16.1），能改 workflow 的人本来就能用这些 secret；服务端只接受自己签发的 token 即可防止凭据被滥用。
