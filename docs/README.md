# displayKvm 文档

项目包含后台、命令行、桌面界面和候选构建流程。rc12 候选已通过软件构建、CI、Linux 双架构载荷门禁与发布下载核验；发布包内程序尚未实机运行，真实显示器和硬件仍待验证。具体边界见[兼容与实测证据](compatibility.md)。

## 用户入口

- [项目入口](../README.md)
- [完整使用说明](usage/README.md)
- [五秒输入测试](usage/input-preview.md)
- [Windows候选说明](usage/windows-candidate.md)
- [兼容与实测证据](compatibility.md)
- [便携包装](../packaging/README.md)

## 开发与技术参考

- [配置契约](contracts.md)
- [构建说明](../build/README.md)
- [Linux随包工具构建契约](development/linux-bundled-tools.md)
- [产品与架构](design.md)：需求、候选架构、平台限制与来源。
- [交互与视觉](ui-design.md)：历史设计草案中的操作流程与视觉建议。
- [文档公开边界](documentation.md)：公共文档与本地记录的边界参考。
- [开发总计划](任务计划/【20261105-显示器切换工具开发】.md)：内部任务、依赖、实施范围与证据追踪。

内部计划和历史验证记录不是用户操作的前置要求。实施记录、独立验证和实际工件证据分别保留；构建或 CI 通过不能替代真实系统与硬件验证。
