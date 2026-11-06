# 已保存目标输入预览实施计划

## 角色、授权与范围

父会话负责协议、授权、派发和收口。后台唯一实施者负责internal/model/management.go、internal/switcher/{queue.go,preview.go及直接测试}、cmd/displaykvmd/runtime.go和必要运行生命周期接点及对应测试。UI实施者独占desktop/**。独立只读验证者执行指定验证并核对证据，不与实施者共用验收角色。

父会话已确认独立入口RED实际编译后以exit 1失败，原因是preview_start返回unsupported_operation，并已放行后台实施。原RED证据保留于docs/verification/2026-11-07-input-preview/backend-entry-red*。禁止修改无关文件、原系列24、旧报告、产物、全局配置或依赖。

## 步骤与完成标准

| 编号 | 范围及依赖 | 预期结果与验收 |
| --- | --- | --- |
| R1 | 新preview_test.go；依赖固定管理协议 | 复用真实管理夹具，用preview_start字面量及原始JSON发已保存目标，断言input_preview后台会话及启动状态；可编译，仅因功能缺失失败。实施者只gofmt及静态读回，独立验证者执行指定RED命令。 |
| R2 | model、switcher及直接测试；依赖R1独立RED与父放行 | 新增固定模型及同worker整次预览；用真实timer和I/O边界通道验证写返回后固定5秒、多屏完整往返、严格预读、跳过、未知写、恢复新鲜读、第三值冲突和恢复失败。保持普通Switch/trial行为。 |
| R3 | runtime及必要生命周期接点/对应测试；依赖R2接口 | 快速启动/状态查询、当前代最新会话、唯一活动预览、深拷贝快照；登记时同锁复检受理并Add，关闭时同锁封闭受理后锁外Wait；验证页面请求取消、Queue.Close、换代及受理竞态，未结束责任不被过期清理。 |
| R4 | 独立只读验收；依赖后台和UI实施均交付 | 按原始需求检查已保存目标测试、固定5秒、真实原输入、多屏顺序、页面关闭、状态真实及既有功能；执行父指定测试，失败项在原授权内补做后复验。 |

R1执行命令：`go test ./cmd/displaykvmd -run '^TestManagementPreviewStartCreatesBackgroundSessionForSavedTarget$' -count=1`。后续测试/构建命令由父会话定向派发；后台实施者不自行扩展验证范围。

## 固定决策与风险

协议使用preview_start/preview_status，Data及会话形状见同日设计文档；不新增cancel、时长参数、持久配置或依赖。Queue.Preview使用既有job/outcome，不能在job内调用公开Switch或ReadInput造成自等待。

登记临界区必须重新确认current、applying、运行代及未关闭受理，并与唯一active及previewWG.Add原子排序；换代/退出先在同锁内关闭受理，再锁外cancel及等待round.done/previewWG，禁止Wait结束后旧请求继续Add。未执行作业由abort发布终态，活动作业由worker完成恢复后发布真实done，不新增生命周期框架。

恢复只在真实预读和新鲜观测支持时写入；取消不能放弃已经登记的恢复责任。Windows切走失联保留失败证据，本轮不扩展枚举/句柄行为，不作硬件保证。完整测试、构建及实机结果仅在实际执行后记录，不把静态读回当作已通过测试。

## 当前交付状态

R2/R3代码与行为测试已落地，待独立验收。Queue.PlanPreview仅复用固定配置规划生成queued逐屏行，不入队或访问硬件；runtime在管理锁外调用，再同锁复检受理并登记。活动会话包括最终progress发布后尚未清active的窗口都不参与过期删除。

新增覆盖包括预读错误/缺值/旧观测零写、有效零值及同输入跳过、每屏独立预算、五秒截止点与慢验证读、整作业排他、unknown不重发且不伪报完成、第三值/失联/恢复错误阻后续、取消与Queue.Close独立恢复、HTTP请求结束继续、真实配置Apply等待物理恢复、并发Start与关闭/Wait，以及进度快照隔离。测试隔离物理I/O，未引入公共测试开关或时钟框架；实施者只执行授权文件gofmt与静态读回，未运行测试、构建、服务或硬件，R4仍待独立验证者执行父指定命令。

## 执行结果（2026-11-07）

实现、定向回归及候选构建包装已完成，证据见[验证摘要](../../verification/2026-11-07-input-preview/summary.md)。实际显示器、GUI与非当前输入DDC能力仍未实机验证，原系列任务24不在本次收尾范围。
