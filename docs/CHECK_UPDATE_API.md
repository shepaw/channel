# 应用版本检查 API 文档

## 概述

`check-update` 端点用于客户端检查是否有新版本可用。该端点无需身份验证，所有客户端都可以调用。

## 端点

### 公开端点：检查更新

```
GET /api/v1/check-update
```

#### 查询参数

| 参数 | 类型 | 必需 | 说明 | 示例 |
|------|------|------|------|------|
| `platform` | string | 是 | 目标平台 | `macos`, `windows`, `linux`, `ios`, `android` |
| `currentVersion` | string | 是 | 当前版本号（格式：X.Y.Z） | `1.0.0` |
| `buildNumber` | integer | 是 | 当前构建号 | `1` |

#### 响应

**HTTP 200 - 有新版本可用**

```json
{
  "version": "1.1.0",
  "buildNumber": 2,
  "description": "Bug fixes and performance improvements",
  "isMandatory": false,
  "releaseDate": "2026-03-24T10:30:00Z",
  "downloadUrl": "https://release.shepaw.com/download/Paw-1.1.0.dmg",
  "fileSize": 52428800,
  "checksum": "sha256:abc123...",
  "minMacOSVersion": "11.0"
}
```

**HTTP 204 - 已是最新版本（无响应体）**

**HTTP 400 - 参数错误**

```json
{
  "error": "invalid parameters"
}
```

**HTTP 500 - 服务器错误**

```json
{
  "error": "failed to check update"
}
```

## 使用示例

### cURL

```bash
# macOS 客户端检查更新
curl -X GET "http://localhost:8080/api/v1/check-update?platform=macos&currentVersion=1.0.0&buildNumber=1"

# Windows 客户端检查更新
curl -X GET "http://localhost:8080/api/v1/check-update?platform=windows&currentVersion=1.0.0&buildNumber=1"

# 已是最新版本
curl -X GET "http://localhost:8080/api/v1/check-update?platform=macos&currentVersion=1.1.0&buildNumber=2"
# 返回 204 No Content
```

### Flutter 客户端示例

```dart
import 'package:http/http.dart' as http;

Future<void> checkForUpdate() async {
  final uri = Uri.parse('https://release.shepaw.com/api/v1/check-update').replace(
    queryParameters: {
      'platform': 'macos',
      'currentVersion': '1.0.0',
      'buildNumber': '1',
    },
  );

  final response = await http.get(uri);

  if (response.statusCode == 200) {
    // 有新版本可用
    final data = jsonDecode(response.body);
    print('新版本: ${data['version']}');
  } else if (response.statusCode == 204) {
    // 已是最新版本
    print('已是最新版本');
  }
}
```

## 管理员 API

### 创建/更新版本

```
POST /admin/api/app-versions
```

需要 Admin 身份验证。

**请求体**

```json
{
  "version": "1.1.0",
  "buildNumber": 2,
  "platform": "macos",
  "description": "Bug fixes and performance improvements",
  "isMandatory": false,
  "releaseDate": "2026-03-24T10:30:00Z",
  "downloadUrl": "https://release.shepaw.com/download/Paw-1.1.0.dmg",
  "fileSize": 52428800,
  "checksum": "sha256:abc123...",
  "minMacOSVersion": "11.0",
  "active": true
}
```

**响应 201 Created**

```json
{
  "id": "av_macos_1",
  "version": "1.1.0",
  "buildNumber": 2,
  "platform": "macos",
  "description": "Bug fixes and performance improvements",
  "isMandatory": false,
  "releaseDate": "2026-03-24T10:30:00Z",
  "downloadUrl": "https://release.shepaw.com/download/Paw-1.1.0.dmg",
  "fileSize": 52428800,
  "checksum": "sha256:abc123...",
  "minMacOSVersion": "11.0",
  "active": true,
  "createdAt": "2026-03-24T12:00:00Z",
  "updatedAt": "2026-03-24T12:00:00Z"
}
```

### 列出所有版本

```
GET /admin/api/app-versions?platform=macos
```

需要 Admin 身份验证。

**查询参数**

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `platform` | string | 否 | 按平台筛选（不提供则返回所有平台） |

**响应 200**

```json
{
  "versions": [
    {
      "id": "av_macos_1",
      "version": "1.1.0",
      "buildNumber": 2,
      "platform": "macos",
      ...
    },
    {
      "id": "av_macos_old",
      "version": "1.0.0",
      "buildNumber": 1,
      "platform": "macos",
      ...
    }
  ]
}
```

### 删除版本

```
DELETE /admin/api/app-versions/:id
```

需要 Admin 身份验证。

**响应 200**

```json
{
  "message": "version deleted"
}
```

## 业务逻辑

### 版本比较规则

版本通过以下规则比较（按优先级）：

1. **主版本号** (Major) - 最重要
2. **次版本号** (Minor)
3. **补丁版本号** (Patch)
4. **构建号** - 相同版本号下，构建号越大越新

示例：

- `1.0.0+1` 有新版本可用时会建议升级到 `1.0.0+2`
- `1.0.0+2` 有新版本可用时会建议升级到 `1.1.0+1`
- `1.1.0+5` 有新版本可用时会建议升级到 `2.0.0+1`

### 活跃版本 (Active)

每个平台同时只能有一个 `active = true` 的版本。

- 创建新版本并设置 `active: true` 时，同平台其他版本自动标记为 `inactive`
- 客户端检查更新时只返回该平台的活跃版本
- 非活跃版本信息保留用于历史记录

### 强制更新 (Mandatory)

- 如果 `isMandatory: true`，客户端应该强制用户更新（不允许跳过/稍后提醒）
- 如果 `isMandatory: false`，客户端可提供"跳过此版本"或"稍后提醒"选项

## 数据库初始化

查看 `scripts/init_app_versions.sql` 了解如何插入测试数据。

```bash
# 使用 psql（PostgreSQL）
psql -U user -d database -f scripts/init_app_versions.sql

# 使用 sqlite3
sqlite3 channel.db < scripts/init_app_versions.sql
```

## 错误处理

| HTTP 状态码 | 说明 | 原因 |
|-------------|------|------|
| 400 | Bad Request | 查询参数格式错误或缺失 |
| 204 | No Content | 已是最新版本或该平台无版本配置 |
| 500 | Internal Server Error | 数据库查询失败或其他服务器错误 |

## 注意事项

1. **请求频率限制**：建议客户端使用本地缓存，避免频繁请求。Flutter 客户端已实现 6 小时冷却。

2. **下载 URL**：`downloadUrl` 可以指向任何合法的 HTTP(S) URL，如：
   - 自托管的更新服务器
   - CDN
   - 应用商店（iOS/Android）
   - GitHub Releases

3. **版本号格式**：严格遵循 `X.Y.Z` 格式，不支持其他格式（如 `1.0.0-rc1`）

4. **构建号**：必须是非负整数

5. **时区**：`releaseDate` 使用 ISO 8601 格式，带时区信息

## 测试命令

### 检查更新存在

```bash
curl -v "http://localhost:8080/api/v1/check-update?platform=macos&currentVersion=1.0.0&buildNumber=1"
# 期望：HTTP 200 + JSON 响应体
```

### 检查已是最新版本

```bash
curl -v "http://localhost:8080/api/v1/check-update?platform=macos&currentVersion=1.1.0&buildNumber=2"
# 期望：HTTP 204 No Content（无响应体）
```

### 参数验证

```bash
curl -v "http://localhost:8080/api/v1/check-update?platform=invalid&currentVersion=1.0.0&buildNumber=1"
# 期望：HTTP 400 + {"error": "invalid parameters"}
```
