# 已保存目标输入预览

## 目标与边界

从本机可见画面发起已保存目标测试：每屏读取真实当前输入，切至目标输入，显示固定5秒，再自动返回预读输入。多屏按配置解析后的顺序逐屏完成完整往返。关闭页面或取消发起请求不取消后台恢复责任。普通Switch及既有trial行为保持不变。

不新增依赖、持久配置、时长参数、取消动作或取消按钮。不写临时配置。桌面界面由独立实施者负责，后台实施者不修改desktop/**。

## 固定管理协议

- `preview_start`：ExpectedGeneration必须匹配；Data为`{"request":{"id":"...","target_id":"...","display_ids":["..."]}}`，只接受已保存目标。对应`ManagementPreviewStartData`，其Request字段为SwitchRequest。
- `preview_status`：同样校验ExpectedGeneration；SessionID非空查询指定会话，为空查询当前运行代最新预览，包含最近终态；无会话返回空Session及空Data。启动应答丢失或页面重载后只查询，不自动重发。
- 响应复用CalibrationSession，Kind为`input_preview`；Data编码InputPreviewStatus。无新取消动作。
- InputPreviewStatus包含state、phase、target_id、active_display_id（可省）、started_at、finished_at（可省）、hold_until（可省）及displays数组。state为queued/running/completed/failed/cancelled；phase为queued/pre_read/switching/holding/restoring/finished。
- 单屏InputPreviewDisplayResult包含display_id、original_input（可省）、target_input、observed_input（可省）、switch_status、restore_status及detail（可省）。输入观测用uint16指针保留有效零值；switch_status为pending/skipped/verified/unknown/failed，restore_status为pending/not_needed/restored/unknown/failed/conflict。

## 执行与恢复

Queue.Preview签名固定为`Preview(ctx context.Context, request model.SwitchRequest, progress func(model.InputPreviewStatus)) (model.InputPreviewStatus, error)`。整次预览复用同一队列作业，在唯一worker内完成；progress同步回调，快照深拷贝切片及指针。入队前取消不操作，入队后必须等真实作业完成恢复再返回。

Queue.PlanPreview只复用已有保存映射规划并返回queued逐屏快照，不枚举、不读写、不启动作业。启动管理请求在锁外调用它，拒绝非法选择并填完整待执行行，然后在管理锁内重新校验受理和登记。

先解析全部已保存映射，再逐屏严格解析设备及预读。不能取得真实原输入则该屏失败且不写；原输入已是目标则skipped/not_needed。首次写前登记恢复责任；测试写同步返回后起算固定5秒，禁止重发切出。验证可复用只读重试，未知写结果保持unknown。

每屏控制阶段使用请求超时预算，保持期不占下一屏预算；验证读的截止点不晚于本屏五秒保持截止点，避免可取消的慢读额外延长测试。原生调用不能抢占时仍等待实际返回，再执行恢复，不遗弃后台硬件协程。

恢复前重新解析设备并新鲜读取：已为原输入则无需写，仍为目标才执行一次恢复写，第三值为conflict且不覆盖。恢复后只有读回原输入才标restored；失败保留实际阶段与原因。取消或Queue.Close提前结束保持期，在同一worker用独立有限context尽力恢复，随后不启动其他屏；禁止另起硬件写goroutine或用已取消context恢复。

## 生命周期

后台会话注册、唯一活动预览及快照由runtimeManager同一互斥锁保护。启动处理器快速应答；完整预览由运行轮次专用WaitGroup跟踪，使用轮次生命context，不依赖请求context，不占用完整5秒的management operations计数。

预览真正登记时，在同一管理锁临界区重新确认round仍为current、applying=false、运行代匹配且该轮未关闭受理，再登记唯一活动会话并执行previewWG.Add。换代和进程退出先在同一锁内封闭该轮新受理，再取消并等待round.done及previewWG.Wait；所有等待均在锁外执行。由同一锁排序Add与关闭受理，防止Wait返回后旧请求再次Add或发布旧轮会话结果。不得仅依赖管理入口解锁前的首次检查。

Queue.Close取消队列生命context并等待worker完成恢复。旧round.done之后，配置换代及进程关停均等待预览WaitGroup及管理operations，再消费最终状态；不得持管理锁等待。活动预览的恢复责任结束前不能因15分钟会话过期而删除，最终progress已含FinishedAt但active尚未释放的窗口也须保留。最终快照落地后清active，再完成WaitGroup登记。

队列中尚未执行的作业由job.abort发布未执行终态；已开始的作业由worker在同一执行流程内完成恢复并发布真实done。恢复不能调用公开入队入口，避免唯一worker等待自身。

## 验收与实机限制

真实管理入口RED测试已由独立验证者执行并确认因未知动作拒绝，父会话已放行产品实现。新增测试使用真实timer、同步通道及外部I/O假实现验证固定5秒、真实输入恢复、页面请求取消、并发启动、Queue.Close、真实配置换代、多屏顺序及冲突保护；不使用time.Sleep猜测次序。后台代码和测试已落地，尚待独立只读验收者执行父指定验证并核对原始需求；静态读回不代表编译或测试通过。

Windows切走后可能因重新枚举而失联，本轮不修改后端枚举或持有旧句柄，不承诺硬件一定能恢复；必须如实报告失败或未知结果。

## 执行结果（2026-11-07）

实现、定向回归及候选构建包装已完成，证据见[验证摘要](../../verification/2026-11-07-input-preview/summary.md)。实际显示器、GUI与非当前输入DDC能力仍未实机验证，原系列任务24不在本次收尾范围。
