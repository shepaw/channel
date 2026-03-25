# 📦 应用版本发布管理指南

## 概述

Paw 应用现已集成完整的版本发布管理系统，包括：

- ✅ **管理后台 UI** - 直观的版本管理界面
- ✅ **版本创建/编辑** - 支持多平台版本信息管理
- ✅ **强制更新** - 标记版本为强制或可选
- ✅ **活跃版本管理** - 每平台仅一个活跃版本
- ✅ **完整的 REST API** - 供客户端和其他工具集成

---

## 访问管理后台

### 1. 管理员登录

访问 `https://release.shepaw.com/admin/login` 或 `http://localhost:8080/admin/login`

**登录方式**：Google OAuth（仅限配置的管理员邮箱）

**配置方式**（环境变量）：
```bash
ADMIN_EMAIL="your-email@gmail.com"
GOOGLE_CLIENT_ID="xxx.apps.googleusercontent.com"
GOOGLE_CLIENT_SECRET="xxx"
BASE_URL="https://release.shepaw.com"  # 用于 OAuth 回调
```

### 2. 进入版本发布管理

登录后，点击左侧导航栏的 **"版本发布"** 或直接访问：
```
https://release.shepaw.com/admin/app-versions
```

---

## 管理后台功能

### 仪表板

**版本统计卡片**：
- 总版本数 - 所有平台的版本总数
- macOS 活跃 - 当前 macOS 平台的活跃版本
- Windows 活跃 - 当前 Windows 平台的活跃版本
- 其他平台 - iOS/Android/Linux 等活跃版本总数

**筛选**：按平台筛选版本列表（iOS/Android/macOS/Windows/Linux）

### 版本列表表格

| 列 | 说明 |
|----|------|
| **版本号** | 版本号（格式：X.Y.Z） |
| **平台** | 目标平台（iOS/Android/macOS/Windows/Linux） |
| **构建号** | 构建号（用于同版本号下的更新） |
| **状态** | 活跃 ✓ / 冻结 ✗ |
| **强制更新** | 强制 / 可选 |
| **发布时间** | ISO 8601 格式的发布日期 |
| **操作** | 编辑 / 删除按钮 |

---

## 创建新版本

### 步骤 1：打开创建表单

点击 **"新建版本"** 按钮

### 步骤 2：填写基本信息

**基本信息** 部分（必填）：

| 字段 | 说明 | 示例 |
|------|------|------|
| 版本号 | 遵循 SemVer X.Y.Z 格式 | 1.1.0 |
| 构建号 | 非负整数，用于同版本号下的微更新 | 2 |
| 平台 | 选择目标平台 | macOS |
| 更新描述 | 用户可见的更新内容说明 | Bug fixes and improvements |

### 步骤 3：填写下载信息

**下载信息** 部分（必填下载链接）：

| 字段 | 说明 | 示例 |
|------|------|------|
| 下载链接 * | 应用下载 URL | https://release.shepaw.com/downloads/Paw-1.1.0.dmg |
| 文件大小 | 文件大小（字节），可选 | 52428800 |
| 校验和 | SHA256 校验和，用于完整性验证 | sha256:abc123... |

### 步骤 4：设置最低版本要求（可选）

根据目标平台填写支持的最低版本：

- **iOS**：最低 iOS 版本（如 14.0）
- **Android**：最低 SDK 级别（如 21）
- **macOS**：最低 macOS 版本（如 11.0）
- **Windows**：最低 Windows 版本（如 10.0）

### 步骤 5：发布设置

**发布设置** 部分：

| 设置 | 说明 |
|------|------|
| **发布时间** | 版本发布的日期和时间（ISO 8601 格式） |
| **强制更新** | ☑️ 勾选后用户无法跳过此版本（适合安全补丁） |
| **设为活跃版本** | ☑️ 勾选后此版本将成为该平台的活跃版本 |

**提示**：
- 如果勾选"设为活跃版本"，同平台现有的活跃版本将自动标记为冻结
- 客户端的 `checkForUpdate()` 仅返回各平台的活跃版本

### 步骤 6：保存

点击 **"保存"** 按钮，系统将验证表单并保存版本信息

**保存成功后**：
- 版本立即出现在列表中
- 如果标记为活跃，客户端下次检查时将看到此版本
- 统计卡片自动更新

---

## 编辑版本

### 修改已发布的版本

1. 在版本列表中找到目标版本
2. 点击行末的 **编辑**（铅笔）图标
3. 修改所需字段
4. 点击 **"保存"** 保存更改

### 可修改的字段

所有字段都可修改，包括：
- 版本号、构建号
- 描述、下载链接
- 强制更新标志
- 活跃状态

**注意**：修改版本号或构建号后，客户端会将其视为新版本

---

## 删除版本

### 删除操作

1. 在版本列表中找到目标版本
2. 点击行末的 **删除**（垃圾桶）图标
3. 确认删除对话框
4. 版本被永久删除

**警告**：删除操作无法撤销，请谨慎操作

---

## 强制更新场景

### 何时使用强制更新

✅ **适合强制更新**：
- 安全漏洞修复
- 重大 Bug 修复导致应用无法使用
- 关键功能更新
- 数据格式破坏性更改

❌ **不适合强制更新**：
- 新功能增加
- UI 改进
- 性能优化
- 普通 Bug 修复

### 设置强制更新

1. 打开创建或编辑表单
2. 勾选 **"强制更新"** 复选框
3. 保存

**客户端表现**：
- 弹出对话框时仅显示 **"立即下载"** 按钮
- 用户无法选择"跳过此版本"或"稍后提醒"
- 应用可通过 `UpdateInfo.isMandatory` 标志获取

---

## 版本比较规则

系统采用标准 SemVer 规则比较版本：

```
版本号比较优先级：主版本 > 次版本 > 补丁版本 > 构建号

示例：
当前版本: 1.0.0+1
新版本可用:

1.0.0+2   ← 仅构建号更新（推荐用于微小修复）
1.1.0+1   ← 次版本更新（新功能）
2.0.0+1   ← 主版本更新（大版本）
```

**规则**：
- 版本 A < 版本 B 时，客户端会提示更新
- 版本号使用 `.` 分隔，最多 3 个数字（X.Y.Z）
- 构建号必须是非负整数
- 同版本号下，构建号越大越新

---

## 活跃版本管理

### 活跃版本概念

- **活跃版本**：客户端 `checkForUpdate()` 返回的版本
- **冻结版本**：历史记录，不主动推荐给用户
- **每平台限制**：同平台同时只能有 **1 个活跃版本**

### 设置活跃版本

**方式 1**：创建新版本时勾选"设为活跃版本"
- 系统自动将同平台其他版本标记为冻结

**方式 2**：编辑已发布的版本
- 改变其"活跃"状态
- 自动处理平台内的版本冲突

### 查看活跃版本

在仪表板顶部的统计卡片中查看：
- macOS 活跃版本数
- Windows 活跃版本数
- 其他平台活跃版本总数

---

## 平台管理

### 支持的平台

| 平台 | 代码 | 说明 |
|------|------|------|
| iOS | `ios` | 苹果移动设备 |
| Android | `android` | 安卓移动设备 |
| macOS | `macos` | 苹果桌面 |
| Windows | `windows` | 微软桌面 |
| Linux | `linux` | Linux 桌面 |

### 为每个平台配置版本

1. 对于每个平台，创建对应的版本
2. 填写该平台特定的"最低版本要求"
3. 设置一个版本为该平台的活跃版本

**示例**：
```
Platform: macOS
Version: 1.1.0
Min macOS Version: 11.0
Download URL: https://release.shepaw.com/Paw-1.1.0.dmg

Platform: Windows
Version: 1.1.0
Min Windows Version: 10.0
Download URL: https://release.shepaw.com/Paw-1.1.0.exe
```

---

## 数据导出/API 访问

### REST API 端点

#### 1. 检查更新（客户端 API）

```http
GET /api/v1/check-update?platform=macos&currentVersion=1.0.0&buildNumber=1
```

**响应**：
- HTTP 200 - 有新版本（JSON 响应）
- HTTP 204 - 已是最新版本（无响应体）
- HTTP 400 - 参数错误

#### 2. 列表查询（Admin API）

```http
GET /admin/api/app-versions?platform=macos
Authorization: admin_session cookie
```

**查询参数**：
- `platform`（可选）：按平台筛选

**响应**：
```json
{
  "versions": [
    {
      "id": "av_macos_1",
      "version": "1.1.0",
      "buildNumber": 2,
      "platform": "macos",
      ...
    }
  ]
}
```

#### 3. 创建版本（Admin API）

```http
POST /admin/api/app-versions
Content-Type: application/json
Authorization: admin_session cookie

{
  "version": "1.1.0",
  "buildNumber": 2,
  "platform": "macos",
  "description": "Bug fixes",
  "downloadUrl": "https://...",
  "isMandatory": false,
  "active": true,
  ...
}
```

#### 4. 删除版本（Admin API）

```http
DELETE /admin/api/app-versions/{id}
Authorization: admin_session cookie
```

---

## 故障排除

### 版本未显示在客户端

**可能原因**：
1. ✗ 版本未标记为活跃 → 编辑版本并勾选"设为活跃版本"
2. ✗ 版本号不高于客户端当前版本 → 检查版本比对规则
3. ✗ 用户跳过此版本 → 清除客户端的 SharedPreferences 中的 `update_skipped_version`

### 客户端无法下载文件

**可能原因**：
1. ✗ 下载链接无效 → 测试链接是否可访问
2. ✗ 文件不存在 → 确保已上传文件到下载服务器
3. ✗ 网络错误 → 检查客户端网络连接

### 无法保存版本

**可能原因**：
1. ✗ 表单验证失败 → 检查所有必填字段
2. ✗ 服务器连接失败 → 确保后端服务运行
3. ✗ 权限问题 → 确保已正确登录为管理员

---

## 最佳实践

### 版本号管理

- **主版本**（X.Y.0）：大版本发布（年度大更新）
- **次版本**（1.Y.0）：功能性更新（季度或月度）
- **补丁版本**（1.1.Z）：Bug 修复（随时发布）

**示例时间表**：
```
2026-01-15: 1.0.0  (初发布)
2026-02-01: 1.0.1  (Bug 修复)
2026-03-01: 1.1.0  (新功能)
2026-03-15: 1.1.1  (Bug 修复)
2026-06-01: 2.0.0  (大版本)
```

### 发布前检查清单

- ☑️ 版本号格式正确（X.Y.Z）
- ☑️ 下载链接可访问
- ☑️ 文件大小和校验和准确
- ☑️ 描述清晰明了
- ☑️ 测试环境已验证
- ☑️ 确认是否需要强制更新

### 灰度发布（计划功能）

未来支持的功能：
- 按用户百分比分阶段推送
- 按地区分阶段发布
- A/B 测试不同版本

---

## 支持和反馈

遇到问题？
- 查看完整 API 文档：`/Users/edenzou/workspace/shepaw/channel/docs/CHECK_UPDATE_API.md`
- 查看数据库初始化脚本：`/Users/edenzou/workspace/shepaw/channel/scripts/init_app_versions.sql`
- 查看源代码：
  - 前端：`/Users/edenzou/workspace/shepaw/channel/templates/admin_app_versions.html`
  - 后端：`/Users/edenzou/workspace/shepaw/channel/pkg/internal/handlers/app_version.go`
