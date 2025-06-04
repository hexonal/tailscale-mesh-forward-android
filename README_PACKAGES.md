# Tailscale Android APK 包分发指南

本文档说明如何使用GitHub Packages来分发和获取Tailscale Android APK包。

## 概述

我们的CI/CD流水线已配置为自动将构建的APK上传到GitHub Packages，支持：

- **Debug APK**: 从main分支自动构建并上传
- **Release APK**: 从其他分支（包括release-branch/*）自动构建并上传

## 包命名规范

### Debug包
- **GroupId**: `com.tailscale.ipn`
- **ArtifactId**: `tailscale-debug`
- **版本格式**: `{版本号}-debug-{commit哈希前7位}`
- **示例**: `1.2.3-debug-abc1234`

### Release包
- **GroupId**: `com.tailscale.ipn`
- **ArtifactId**: `tailscale-release`
- **版本格式**: `{版本号}-{commit哈希前7位}`
- **示例**: `1.2.3-def5678`

## 如何获取APK包

### 1. 通过GitHub Web界面

1. 访问仓库主页
2. 点击右侧的 "Packages" 链接
3. 选择所需的包（tailscale-debug 或 tailscale-release）
4. 选择版本并下载APK文件

### 2. 通过Maven配置

在您的 `build.gradle` 文件中添加：

```gradle
repositories {
    maven {
        name = "GitHubPackages"
        url = uri("https://maven.pkg.github.com/YOUR_USERNAME/YOUR_REPO_NAME")
        credentials {
            username = project.findProperty("gpr.user") ?: System.getenv("USERNAME")
            password = project.findProperty("gpr.key") ?: System.getenv("TOKEN")
        }
    }
}

dependencies {
    // Debug版本
    implementation 'com.tailscale.ipn:tailscale-debug:版本号'
    
    // 或者Release版本
    implementation 'com.tailscale.ipn:tailscale-release:版本号'
}
```

### 3. 通过curl命令下载

```bash
# 获取可用版本列表
curl -H "Authorization: Bearer YOUR_TOKEN" \
     https://maven.pkg.github.com/YOUR_USERNAME/YOUR_REPO_NAME/com/tailscale/ipn/tailscale-debug/maven-metadata.xml

# 下载特定版本的Debug APK
curl -H "Authorization: Bearer YOUR_TOKEN" \
     -L -o tailscale-debug.apk \
     https://maven.pkg.github.com/YOUR_USERNAME/YOUR_REPO_NAME/com/tailscale/ipn/tailscale-debug/VERSION/tailscale-debug-VERSION.apk

# 下载特定版本的Release APK
curl -H "Authorization: Bearer YOUR_TOKEN" \
     -L -o tailscale-release.apk \
     https://maven.pkg.github.com/YOUR_USERNAME/YOUR_REPO_NAME/com/tailscale/ipn/tailscale-release/VERSION/tailscale-release-VERSION.apk
```

## 权限配置

### 对于仓库维护者

CI流水线使用 `GITHUB_TOKEN` 自动获得上传权限，无需额外配置。

### 对于下载用户

需要配置GitHub个人访问令牌（PAT）：

1. 访问 [GitHub Settings > Developer settings > Personal access tokens](https://github.com/settings/tokens)
2. 创建新的Classic PAT或Fine-grained PAT
3. 为PAT授予以下权限：
   - `read:packages` - 读取包的权限
   - `repo` - 访问私有仓库（如果仓库是私有的）

## 自动化构建触发

### Debug构建
- **触发条件**: 推送到 `main` 分支
- **输出**: Debug APK上传到GitHub Packages
- **命名**: `tailscale-debug:{version}-debug-{short_sha}`

### Release构建
- **触发条件**: 推送到除 `main` 之外的任何分支（如 `release-branch/*`、`feature/*` 等）
- **输出**: Release APK上传到GitHub Packages
- **命名**: `tailscale-release:{version}-{short_sha}`

### Pull Request构建
- **触发条件**: 创建或更新Pull Request
- **输出**: 仅构建APK用于测试，不上传到GitHub Packages
- **构建类型**: 根据目标分支决定（main分支为debug，其他分支为release）

## 版本管理

版本号自动从Makefile的 `make version` 命令获取，格式包含：
- 应用版本号
- 构建类型（debug/release）
- Git commit短哈希

这确保了每个构建都有唯一的版本标识符。

## 分支策略说明

| 分支类型 | 构建类型 | 上传到Packages | 说明 |
|---------|---------|---------------|------|
| main | Debug | ✅ | 开发版本，包含调试信息 |
| release-branch/* | Release | ✅ | 发布版本，优化构建 |
| feature/* | Release | ✅ | 功能分支，使用release构建 |
| hotfix/* | Release | ✅ | 热修复分支，使用release构建 |
| Pull Request | Debug/Release | ❌ | 仅用于测试，不上传 |

## 故障排除

### 常见问题

1. **权限被拒绝**
   - 确保您的GitHub令牌有正确的权限
   - 检查仓库的包访问设置

2. **包未找到**
   - 验证包名和版本号是否正确
   - 确认构建是否成功完成

3. **下载失败**
   - 检查网络连接
   - 验证认证凭据

### 调试步骤

1. 检查GitHub Actions构建日志
2. 验证包是否在GitHub Packages中可见
3. 测试使用不同的认证方法

## 安全注意事项

- 不要在公共代码中硬编码GitHub令牌
- 使用环境变量或安全的配置管理
- 定期轮换访问令牌
- 为生产环境使用最小权限原则

## 联系支持

如果遇到问题，请：
1. 检查本文档的故障排除部分
2. 查看GitHub Actions构建日志
3. 在仓库中创建issue描述问题 