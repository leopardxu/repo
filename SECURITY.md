# 安全策略 Security Policy

## 支持版本 / Supported Versions

仅对最新的 `main` 分支与最新发布的 Release 提供安全修复。
/ Security fixes are only provided for the latest `main` branch and the latest release.

| 版本 / Version | 支持状态 / Supported |
| --- | --- |
| latest release | :white_check_mark: |
| older releases | :x: |

## 报告漏洞 / Reporting a Vulnerability

**请勿通过公开 Issue 报告安全漏洞。**
**Please do NOT report security vulnerabilities through public GitHub issues.**

请使用 GitHub 的私密漏洞报告功能：

1. 前往仓库的 **Security** 标签页 → **Report a vulnerability**；
2. 或通过 [New security advisory](https://github.com/leopardxu/repo-go/security/advisories/new) 私密提交。

Please use GitHub's private vulnerability reporting:
<https://github.com/leopardxu/repo-go/security/advisories/new>

提交时请尽量包含：

- 受影响的版本 / commit；
- 复现步骤或概念验证（PoC）；
- 影响范围评估。

Include the affected version/commit, reproduction steps, and impact assessment.

## 响应时限 / Response Time

通常在 **72 小时内**确认收到，并同步评估与修复进度。
/ Acknowledgment within 72 hours; progress updates as the fix is developed.

## 范围说明 / Scope Notes

`repo-go` 是一个操作本地 Git 仓库并调用系统 `git` 的命令行工具，重点关注以下类别的问题：

- 清单（manifest）解析：XML 外部实体注入（XXE）、路径穿越、不可信清单仓库导致的命令注入；
- 子进程执行：通过清单或配置注入 `git` 参数；
- 凭据处理：`--manifest-server-password` 等凭据在日志/错误信息中泄漏；
- `selfupdate` 下载链路的完整性与校验。

`repo-go` shells out to the system `git`. Areas of particular interest: manifest parsing (XXE, path traversal), argument injection into `git`, credential leakage in logs, and `selfupdate` download integrity.
