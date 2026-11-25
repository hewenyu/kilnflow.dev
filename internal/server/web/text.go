package web

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 页面数据中的文字（多语言，设计见 docs/I18N.md）：
//   - 服务端不按语言拼文案，只给出消息（msgID 加参数），前端按当前语言从词典（web/src/i18n/locales/*.json）取文案，
//     切换语言时立即更新；时间、数量等参数传原始值（Unix 秒、数字），由前端按语言格式化。
//   - 仓库名、登录名、对象名等原始内容作为参数或原样显示的文字（rawText）传递，不当作消息键。
//   - 还没有迁移的页面照常传字符串：uiText 的原样文字编码为 JSON 字符串，与原来的页面数据相同。
// 消息键和参数必须在 messages 中登记：契约测试（text_test.go）检查每条消息在两种语言的词典中都有、参数名和格式一致；
// 跳转后的提示（?flash=）只接受登记为 flash 的消息和参数。

// msgID 是前端词典中的消息键。
type msgID string

// paramKind 是消息参数的类型：文字中按 {name} 插入；数字可以按 {name, number}（数字格式）、{name, time}（Unix 秒）格式化。
type paramKind int

const (
	paramString paramKind = iota + 1
	paramInt
)

// msgSpec 是一条消息的参数和用途。
type msgSpec struct {
	params map[string]paramKind
	// flash 表示可以作为跳转后的提示放进地址（?flash=）。
	flash bool
}

// 服务端给出的消息。新增消息时在 messages 中登记，并在两种语言的词典中加上文案。
const (
	msgLoginTitle         msgID = "login.pageTitle"
	msgLoginNoticeExpired msgID = "login.noticeExpired"
	msgLoginNoticeRevoked msgID = "login.noticeRevoked"
	msgLoginNoticeLogout  msgID = "login.noticeLogout"
	msgLoginErrAdminOff   msgID = "login.errTokenDisabled"
	msgLoginErrAdminEmpty msgID = "login.errTokenEmpty"
	msgLoginErrAdminWrong msgID = "login.errTokenInvalid"

	msgErrLinkRetry          msgID = "error.linkRetry"
	msgErrLinkBack           msgID = "error.linkBack"
	msgErrLinkRuns           msgID = "error.linkRuns"
	msgErrLinkLogin          msgID = "error.linkLogin"
	msgErrLinkLoginAsAdmin   msgID = "error.linkLoginAsAdmin"
	msgErrLinkBackToLogin    msgID = "error.linkBackToLogin"
	msgErrLinkRelogin        msgID = "error.linkRelogin"
	msgErrLinkGitHubRelogin  msgID = "error.linkGitHubRelogin"
	msgErrLinkGitHubRetry    msgID = "error.linkGitHubRetry"
	msgErrLinkAccount        msgID = "error.linkAccount"
	msgErrServerHeading      msgID = "error.serverHeading"
	msgErrServerReasonGet    msgID = "error.serverReasonGet"
	msgErrServerReasonPost   msgID = "error.serverReasonPost"
	msgErrNotFoundHeading    msgID = "error.notFoundHeading"
	msgErrNotFoundReason     msgID = "error.notFoundReason"
	msgErrBadRequestHeading  msgID = "error.badRequestHeading"
	msgErrForbiddenHeading   msgID = "error.forbiddenHeading"
	msgErrForbiddenReason    msgID = "error.forbiddenReason"
	msgErrCSRFHeading        msgID = "error.csrfHeading"
	msgErrCSRFReason         msgID = "error.csrfReason"
	msgErrOAuthOffHeading    msgID = "error.oauthDisabledHeading"
	msgErrOAuthOffReason     msgID = "error.oauthDisabledReason"
	msgErrOAuthBadHeading    msgID = "error.oauthInvalidHeading"
	msgErrOAuthBadReason     msgID = "error.oauthInvalidReason"
	msgErrOAuthBrowserReason msgID = "error.oauthOtherBrowserReason"
	msgErrOAuthExpHeading    msgID = "error.oauthExpiredHeading"
	msgErrOAuthExpReason     msgID = "error.oauthExpiredReason"
	msgErrOAuthDenyHeading   msgID = "error.oauthDeniedHeading"
	msgErrOAuthDenyReason    msgID = "error.oauthDeniedReason"
	msgErrOAuthNoAccHeading  msgID = "error.oauthNoAccessHeading"
	msgErrOAuthNoAccReason   msgID = "error.oauthNoAccessReason"
	msgErrOAuthFailHeading   msgID = "error.oauthFailedHeading"
	msgErrOAuthFailReason    msgID = "error.oauthFailedReason"
	msgErrBindBadHeading     msgID = "error.bindInvalidHeading"
	msgErrBindBadReason      msgID = "error.bindInvalidReason"

	msgAccountBound   msgID = "account.bound"
	msgAccountUnbound msgID = "account.unbound"

	// 操作失败时的通用说明（内部错误只给错误编号，见 failureText）。
	msgActionInternal msgID = "action.internalError"
	// 页面脚本调用的接口：未登录。
	msgAPIUnauthorized msgID = "api.unauthorized"
)

// 总览与 CI 运行（总览、运行列表、启动构建、run 和 job 详情、日志）的消息。
const (
	msgNavOverview msgID = "nav.overview"
	msgNavRuns     msgID = "nav.runs"

	msgSetupReposTitle    msgID = "overview.setupReposTitle"
	msgSetupReposText     msgID = "overview.setupReposText"
	msgSetupReposAction   msgID = "overview.setupReposAction"
	msgSetupRunnerTitle   msgID = "overview.setupRunnerTitle"
	msgSetupRunnerText    msgID = "overview.setupRunnerText"
	msgSetupRunnerAction  msgID = "overview.setupRunnerAction"
	msgSetupBindTitle     msgID = "overview.setupBindTitle"
	msgSetupBindText      msgID = "overview.setupBindText"
	msgSetupBindAction    msgID = "overview.setupBindAction"
	msgSetupTokenTitle    msgID = "overview.setupTokenTitle"
	msgSetupTokenText     msgID = "overview.setupTokenText"
	msgSetupTokenAction   msgID = "overview.setupTokenAction"
	msgRunStateQueued     msgID = "runState.queued"
	msgRunStateActive     msgID = "runState.active"
	msgRunStateSucceeded  msgID = "runState.succeeded"
	msgRunStateFailed     msgID = "runState.failed"
	msgRunStateCancelled  msgID = "runState.cancelled"
	msgRunStateSkipped    msgID = "runState.skipped"
	msgRunNewTitle        msgID = "runNew.pageTitle"
	msgRunNewPickRepo     msgID = "runNew.errPickRepo"
	msgRunNewRepoMissing  msgID = "runNew.errRepoMissing"
	msgRunNewPickWorkflow msgID = "runNew.errPickWorkflow"
	msgRunNewWfMissing    msgID = "runNew.errWorkflowMissing"
	msgRunNewWfNoDispatch msgID = "runNew.errNotDispatchable"
	msgRunNewRefNotFound  msgID = "runNew.errRefNotFound"
	msgRunNewInputs       msgID = "runNew.errInputs"
	msgRunNewDispatched   msgID = "runNew.dispatched"

	msgInputNoEquals      msgID = "runNew.inputNoEquals"
	msgInputNoName        msgID = "runNew.inputNoName"
	msgInputBadName       msgID = "runNew.inputBadName"
	msgInputDuplicate     msgID = "runNew.inputDuplicate"
	msgInputMissing       msgID = "runNew.inputMissing"
	msgInputNotBool       msgID = "runNew.inputNotBoolean"
	msgInputNotChoice     msgID = "runNew.inputNotChoice"
	msgInputUndeclared    msgID = "runNew.inputUndeclared"
	msgRunTitle           msgID = "run.pageTitle"
	msgRunTitleFirst      msgID = "run.pageTitleFirst"
	msgRunTitleRerun      msgID = "run.pageTitleRerun"
	msgRunNotFound        msgID = "run.notFound"
	msgRunActDispatch     msgID = "run.activityDispatch"
	msgRunActCancel       msgID = "run.activityCancel"
	msgRunActRerun        msgID = "run.activityRerun"
	msgRunActRerunFailed  msgID = "run.activityRerunFailed"
	msgRunActRerunAll     msgID = "run.activityRerunAll"
	msgRunActRerunNoCache msgID = "run.activityRerunNoCache"
	msgRunActRerunJob     msgID = "run.activityRerunJob"
	msgRunCancelled       msgID = "run.cancelRequested"
	msgRunCancelFailed    msgID = "run.cancelFailed"
	msgRunCancelFailedRef msgID = "run.cancelFailedRef"
	msgRunRerunCreated    msgID = "run.rerunCreated"
	msgRunRerunFailed     msgID = "run.rerunFailed"
	msgRunRerunFailedRef  msgID = "run.rerunFailedRef"

	msgJobNotFound          msgID = "job.notFound"
	msgJobRerunNotFound     msgID = "job.rerunNotFound"
	msgJobAttemptPublish    msgID = "job.attemptPublish"
	msgJobAttemptInvalid    msgID = "job.attemptInvalid"
	msgJobAttemptMissing    msgID = "job.attemptMissing"
	msgJobOutputGone        msgID = "job.outputGone"
	msgJobLogPurged         msgID = "job.logPurged"
	msgJobLogNoOutput       msgID = "job.logNoOutput"
	msgJobLogPublishPending msgID = "job.logPublishPending"
	msgJobLogReused         msgID = "job.logReused"
	msgJobLogCached         msgID = "job.logCached"
	msgJobLogSkipped        msgID = "job.logSkipped"
	msgJobLogQueued         msgID = "job.logQueued"
	msgJobLogNotRun         msgID = "job.logNotRun"
	msgJobLogNone           msgID = "job.logNone"

	msgLogBadAttempt          msgID = "log.errBadAttempt"
	msgLogBadOffset           msgID = "log.errBadOffset"
	msgLogJobNotFound         msgID = "log.errJobNotFound"
	msgLogJobAttemptNotFound  msgID = "log.errJobAttemptNotFound"
	msgLogTaskNotFound        msgID = "log.errTaskNotFound"
	msgLogTaskAttemptNotFound msgID = "log.errTaskAttemptNotFound"
	msgLogServer              msgID = "log.errServer"
)

// CI Runner 与 AI 执行节点（列表、详情、注册、本地缓存、按台升级）的消息。
const (
	msgRunnersTitle     msgID = "runners.pageTitle"
	msgRunnerTitle      msgID = "runner.pageTitle"
	msgRunnerCacheTitle msgID = "runnerCache.pageTitle"
	msgRunnerNewTitle   msgID = "runnerNew.pageTitle"
	msgAIRunnersTitle   msgID = "aiRunners.pageTitle"
	msgAIRunnerTitle    msgID = "aiRunner.pageTitle"
	msgRunnerNotFound   msgID = "runner.notFound"
	msgAIRunnerNotFound msgID = "aiRunner.notFound"
	msgRunnerOpUnknown  msgID = "runner.opUnknown"

	msgRunnerFilterOnline    msgID = "runnerFilter.online"
	msgRunnerFilterAccepting msgID = "runnerFilter.accepting"
	msgRunnerFilterPaused    msgID = "runnerFilter.paused"
	msgRunnerFilterBusy      msgID = "runnerFilter.busy"
	msgRunnerFilterOffline   msgID = "runnerFilter.offline"
	msgRunnerFilterRevoked   msgID = "runnerFilter.revoked"

	msgRunnerOpNotFound       msgID = "runnerOp.notFound"
	msgRunnerOpNotRevoked     msgID = "runnerOp.notRevoked"
	msgRunnerOpRevokedAlready msgID = "runnerOp.alreadyRevoked"
	msgRunnerOpFailed         msgID = "runnerOp.failed"
	msgRunnerOpFailedRef      msgID = "runnerOp.failedRef"
	msgRunnerOpDrained        msgID = "runnerOp.drained"
	msgRunnerOpDrainedBusy    msgID = "runnerOp.drainedBusy"
	msgRunnerOpUndrained      msgID = "runnerOp.undrained"
	msgRunnerOpUndrainedOff   msgID = "runnerOp.undrainedOffline"
	msgRunnerOpDeleted        msgID = "runnerOp.deleted"
	msgRunnerOpRevoked        msgID = "runnerOp.revoked"
	msgRunnerOpRevokedBusy    msgID = "runnerOp.revokedBusy"
	msgRunnerOpRevokedBusyAI  msgID = "runnerOp.revokedBusyAI"

	msgRunnerNewErrKind msgID = "runnerNew.errKind"
	msgRunnerNewErrPool msgID = "runnerNew.errPool"
	msgRunnerNewErrName msgID = "runnerNew.errName"

	msgUpgradeOpUnknown       msgID = "runnerUpgrade.opUnknown"
	msgUpgradeRequested       msgID = "runnerUpgrade.requested"
	msgUpgradeRequestedOff    msgID = "runnerUpgrade.requestedOffline"
	msgUpgradeAlready         msgID = "runnerUpgrade.alreadyRequested"
	msgUpgradeCancelled       msgID = "runnerUpgrade.cancelled"
	msgUpgradeCheckRequested  msgID = "runnerUpgrade.checkRequested"
	msgUpgradeStartFailed     msgID = "runnerUpgrade.startFailed"
	msgUpgradeStartFailedRef  msgID = "runnerUpgrade.startFailedRef"
	msgUpgradeCancelFailed    msgID = "runnerUpgrade.cancelFailed"
	msgUpgradeCancelFailedRef msgID = "runnerUpgrade.cancelFailedRef"
	msgUpgradeCheckFailed     msgID = "runnerUpgrade.checkFailed"
	msgUpgradeCheckFailedRef  msgID = "runnerUpgrade.checkFailedRef"
	msgUpgradeRunnerMissing   msgID = "runnerUpgrade.runnerMissing"

	msgUpgradeReasonAINode        msgID = "upgradeReason.aiNode"
	msgUpgradeReasonRevoked       msgID = "upgradeReason.revoked"
	msgUpgradeReasonUnsupported   msgID = "upgradeReason.unsupported"
	msgUpgradeReasonNoLauncher    msgID = "upgradeReason.noLauncher"
	msgUpgradeReasonDisabled      msgID = "upgradeReason.disabled"
	msgUpgradeReasonCheckFailed   msgID = "upgradeReason.checkFailed"
	msgUpgradeReasonNotChecked    msgID = "upgradeReason.notChecked"
	msgUpgradeReasonUpToDate      msgID = "upgradeReason.upToDate"
	msgUpgradeReasonNewer         msgID = "upgradeReason.newer"
	msgUpgradeReasonUpgrading     msgID = "upgradeReason.upgrading"
	msgUpgradeReasonLatestChanged msgID = "upgradeReason.latestChanged"
	msgUpgradeReasonNoPending     msgID = "upgradeReason.noPending"
	msgUpgradeReasonSwitching     msgID = "upgradeReason.switching"
	msgUpgradeReasonUnknown       msgID = "upgradeReason.unknown"

	msgCacheFormInvalid    msgID = "runnerCache.formInvalid"
	msgCachePurgeRequested msgID = "runnerCache.purgeRequested"
	msgCachePurgeFailed    msgID = "runnerCache.purgeFailed"
	msgCachePurgeFailedRef msgID = "runnerCache.purgeFailedRef"
	msgCacheRepoNotFound   msgID = "runnerCache.errRepoNotFound"
	msgCacheNoteNoReport   msgID = "cacheNote.noReport"
	msgCacheNoteReport     msgID = "cacheNote.report"
	msgCacheNoteBusy       msgID = "cacheNote.busy"
	msgCacheNoteSharedKept msgID = "cacheNote.sharedKept"
	msgCacheNoteUnknown    msgID = "cacheNote.unknown"
)

// AI 审计请求与任务、AI 开发工作项、设计依据（外部文档）的消息。
const (
	msgAIRequestsTitle      msgID = "aiRequests.pageTitle"
	msgAITypeReview         msgID = "aiType.review"
	msgAITypeDevelop        msgID = "aiType.develop"
	msgAIReqStateQueued     msgID = "aiRequestState.queued"
	msgAIReqStateActive     msgID = "aiRequestState.active"
	msgAIReqStateSucceeded  msgID = "aiRequestState.succeeded"
	msgAIReqStateFailed     msgID = "aiRequestState.failed"
	msgAIReqStateCancelled  msgID = "aiRequestState.cancelled"
	msgAIReqStateSkipped    msgID = "aiRequestState.skipped"
	msgAIRequestTitleReview msgID = "aiRequest.pageTitleReview"
	msgAIRequestTitleDev    msgID = "aiRequest.pageTitleDevelop"
	msgAIRequestCancelled   msgID = "aiRequest.cancelRequested"
	msgAIRequestRerun       msgID = "aiRequest.rerunCreated"
	msgAITaskTitleReview    msgID = "aiTask.pageTitleReview"
	msgAITaskTitleDev       msgID = "aiTask.pageTitleDevelop"
	msgAITaskNotFound       msgID = "aiTask.notFound"
	msgAITaskRerunNotFound  msgID = "aiTask.rerunNotFound"
	msgAITaskAttemptInvalid msgID = "aiTask.attemptInvalid"
	msgAITaskAttemptMissing msgID = "aiTask.attemptMissing"
	msgAITaskLogPurged      msgID = "aiTask.logPurged"
	msgAITaskLogNoOutput    msgID = "aiTask.logNoOutput"
	msgAITaskLogReused      msgID = "aiTask.logReused"
	msgAITaskLogSkipped     msgID = "aiTask.logSkipped"
	msgAITaskLogNotRun      msgID = "aiTask.logNotRun"

	msgAIDevTitle          msgID = "aiDev.pageTitle"
	msgAIDevTitleRepo      msgID = "aiDev.pageTitleRepo"
	msgAIDevRepoNotFound   msgID = "aiDev.repoNotFound"
	msgAIDevItemTitle      msgID = "aiDevItem.pageTitle"
	msgAIDevItemNotFound   msgID = "aiDevItem.notFound"
	msgAIDevSyncNoRepo     msgID = "aiDev.syncNoRepo"
	msgAIDevSyncFailed     msgID = "aiDev.syncFailed"
	msgAIDevSyncFailedRef  msgID = "aiDev.syncFailedRef"
	msgAIDevSynced         msgID = "aiDev.synced"
	msgAIDevSyncedPartial  msgID = "aiDev.syncedPartial"
	msgAIDevPauseNoRepo    msgID = "aiDev.pauseNoRepo"
	msgAIDevResumeNoRepo   msgID = "aiDev.resumeNoRepo"
	msgAIDevPauseFailed    msgID = "aiDev.pauseFailed"
	msgAIDevPauseFailedRef msgID = "aiDev.pauseFailedRef"
	msgAIDevResumeFailed   msgID = "aiDev.resumeFailed"
	msgAIDevResumeFailRef  msgID = "aiDev.resumeFailedRef"
	msgAIDevPaused         msgID = "aiDev.paused"
	msgAIDevResumed        msgID = "aiDev.resumed"
	msgAIDevStopBadID      msgID = "aiDev.stopBadID"
	msgAIDevRetryBadID     msgID = "aiDev.retryBadID"
	msgAIDevStopFailed     msgID = "aiDev.stopFailed"
	msgAIDevStopFailedRef  msgID = "aiDev.stopFailedRef"
	msgAIDevRetryFailed    msgID = "aiDev.retryFailed"
	msgAIDevRetryFailedRef msgID = "aiDev.retryFailedRef"
	msgAIDevStopped        msgID = "aiDev.stopped"
	msgAIDevRetried        msgID = "aiDev.retried"
	msgAIDevItemMissing    msgID = "aiDev.itemMissing"
	msgAIDevRepoMissing    msgID = "aiDev.repoMissing"
	msgAIDevNotRetryable   msgID = "aiDev.notRetryable"
	msgAIDevFinished       msgID = "aiDev.finished"

	msgIntegrityCount     msgID = "docsIntegrity.countMismatch"
	msgIntegrityDocDigest msgID = "docsIntegrity.docDigestInvalid"
	msgIntegrityManifest  msgID = "docsIntegrity.manifestInvalid"
	msgIntegrityMismatch  msgID = "docsIntegrity.manifestMismatch"

	msgIssueSetMissing        msgID = "docsIssue.snapshotSetMissing"
	msgIssueSetOtherRepo      msgID = "docsIssue.snapshotSetOtherRepo"
	msgIssueSetDigest         msgID = "docsIssue.snapshotSetDigest"
	msgIssueReqSetDigest      msgID = "docsIssue.requestSetDigest"
	msgIssueReqItemSnapshot   msgID = "docsIssue.requestWorkItemSnapshot"
	msgIssueReqItemDigest     msgID = "docsIssue.requestWorkItemDigest"
	msgIssueReqSourceUnknown  msgID = "docsIssue.requestSourceUnknown"
	msgIssueBasisDigest       msgID = "docsIssue.basisDigestInvalid"
	msgIssueTaskItemMissing   msgID = "docsIssue.taskWorkItemMissing"
	msgIssueTaskItemOtherRepo msgID = "docsIssue.taskWorkItemOtherRepo"
	msgIssueTaskItemSnapshot  msgID = "docsIssue.taskWorkItemSnapshot"
	msgIssueTaskDevRequest    msgID = "docsIssue.taskDevRequestMismatch"
	msgIssueTaskNoWorkItem    msgID = "docsIssue.taskNoWorkItem"
	msgIssueTaskClaim         msgID = "docsIssue.taskClaimMismatch"
	msgIssueTaskSpec          msgID = "docsIssue.taskSpecMismatch"
	msgIssueTaskSpecDigest    msgID = "docsIssue.taskSpecDigest"
	msgIssueTaskRefsMismatch  msgID = "docsIssue.taskRefsMismatch"
	msgIssueTaskRefsExtra     msgID = "docsIssue.taskRefsUnexpected"

	msgNoteBasisConflict      msgID = "docsNote.basisConflict"
	msgNoteBasisUnverifiable  msgID = "docsNote.basisUnverifiable"
	msgNoteOtherWorkItem      msgID = "docsNote.otherWorkItem"
	msgNoteLegacyDigestPinned msgID = "docsNote.legacyDigestPinned"
	msgNoteLegacyRefsMismatch msgID = "docsNote.legacyRefsMismatch"
	msgNoteLegacyNoneOK       msgID = "docsNote.legacyNoneConfirmed"
	msgNoteLegacyNoneRefs     msgID = "docsNote.legacyNoneRefs"
	msgNoteClaimedDynamic     msgID = "docsNote.claimedDynamic"
	msgNoteLegacyPinned       msgID = "docsNote.legacyPinned"
	msgNoteLegacyNoBasis      msgID = "docsNote.legacyNoBasis"
	msgNoteLegacyInsufficient msgID = "docsNote.legacyInsufficient"
	msgNoteDeclaredNone       msgID = "docsNote.declaredNone"
	msgNoteUnknown            msgID = "docsNote.unknown"
	msgNoteWorkItemNoBasis    msgID = "docsNote.workItemNoBasis"
	msgNoteNoRefs             msgID = "docsNote.noRefs"
	msgNoteSameEmpty          msgID = "docsNote.sameEmpty"
	msgNoteSameBasis          msgID = "docsNote.sameBasis"
	msgNoteItemEmptyNoDigest  msgID = "docsNote.workItemEmptyNoDigest"
	msgNoteItemEmptyDigest    msgID = "docsNote.workItemEmptyDigest"
	msgNoteItemRefsMismatch   msgID = "docsNote.workItemRefsMismatch"
	msgNoteItemRefsNoDigest   msgID = "docsNote.workItemRefsNoDigest"
	msgNoteItemDigestMismatch msgID = "docsNote.workItemDigestMismatch"
	msgNoteSetEmptyNoDigest   msgID = "docsNote.setEmptyNoDigest"
	msgNoteSetEmptyDigest     msgID = "docsNote.setEmptyDigest"
	msgNoteSetRefsMismatch    msgID = "docsNote.setRefsMismatch"
	msgNoteSetRefsNoDigest    msgID = "docsNote.setRefsNoDigest"
	msgNoteSetDigestMismatch  msgID = "docsNote.setDigestMismatch"
)

// 合并队列与自动打 tag 的消息（页面标题、操作后的提示、队列项的状态和原因、自动打 tag 的原因，见 merge_queue_text.go）。
const (
	msgMQTitle        msgID = "mergeQueue.pageTitle"
	msgMQTitleRepo    msgID = "mergeQueue.pageTitleRepo"
	msgMQRepoNotFound msgID = "mergeQueue.repoNotFound"

	msgMQAddNoRepo        msgID = "mergeQueue.addNoRepo"
	msgMQAddBadPR         msgID = "mergeQueue.addBadPR"
	msgMQAddFailed        msgID = "mergeQueue.addFailed"
	msgMQAddFailedRef     msgID = "mergeQueue.addFailedRef"
	msgMQAddNoForge       msgID = "mergeQueue.addNoForge"
	msgMQAddRepoMissing   msgID = "mergeQueue.addRepoMissing"
	msgMQAlreadyQueued    msgID = "mergeQueue.alreadyQueued"
	msgMQAdded            msgID = "mergeQueue.added"
	msgMQRemoveNoRepo     msgID = "mergeQueue.removeNoRepo"
	msgMQRemoveBadPR      msgID = "mergeQueue.removeBadPR"
	msgMQRemoveFailed     msgID = "mergeQueue.removeFailed"
	msgMQRemoveFailedRef  msgID = "mergeQueue.removeFailedRef"
	msgMQRemoveNoForge    msgID = "mergeQueue.removeNoForge"
	msgMQRemoveRepoMiss   msgID = "mergeQueue.removeRepoMissing"
	msgMQRemoveRequested  msgID = "mergeQueue.removeRequested"
	msgATRetryMissing     msgID = "autoTag.retryMissing"
	msgATNotRetryable     msgID = "autoTag.notRetryable"
	msgATRetryFailed      msgID = "autoTag.retryFailed"
	msgATRetryFailedRef   msgID = "autoTag.retryFailedRef"
	msgATRetryRepoMissing msgID = "autoTag.retryRepoMissing"
	msgATRetried          msgID = "autoTag.retried"

	msgMQStatusWaitPlan         msgID = "mqStatus.waitPlan"
	msgMQStatusConfigError      msgID = "mqStatus.configError"
	msgMQStatusUpdating         msgID = "mqStatus.updating"
	msgMQStatusWaitRuns         msgID = "mqStatus.waitRuns"
	msgMQStatusPosition         msgID = "mqStatus.position"
	msgMQStatusPosBranch        msgID = "mqStatus.positionBranch"
	msgMQStatusRemoving         msgID = "mqStatus.removing"
	msgMQStatusMerging          msgID = "mqStatus.merging"
	msgMQStatusUpdateRetry      msgID = "mqStatus.updateRetry"
	msgMQStatusConflictRead     msgID = "mqStatus.conflictReadFailed"
	msgMQStatusMergeability     msgID = "mqStatus.mergeability"
	msgMQStatusBaseRead         msgID = "mqStatus.baseReadFailed"
	msgMQStatusConflictRec      msgID = "mqStatus.conflictRecording"
	msgMQStatusClosingIssues    msgID = "mqStatus.closingIssues"
	msgMQStatusIssuesClosed     msgID = "mqStatus.issuesClosed"
	msgMQStatusIssuesUnknown    msgID = "mqStatus.issuesUnknown"
	msgMQStatusIssuesBoth       msgID = "mqStatus.issuesClosedUnknown"
	msgMQStatusIssuesRetry      msgID = "mqStatus.issuesRetry"
	msgMQStatusIssuesRetryDeny  msgID = "mqStatus.issuesRetryDenied"
	msgMQStatusIssuesGaveUp     msgID = "mqStatus.issuesGaveUp"
	msgMQStatusIssuesGaveUpDeny msgID = "mqStatus.issuesGaveUpDenied"
	msgMQStatusIssuesInactive   msgID = "mqStatus.issuesInactive"

	msgMQReasonPRClosed          msgID = "mqReason.prClosed"
	msgMQReasonLabelRemoved      msgID = "mqReason.labelRemoved"
	msgMQReasonFork              msgID = "mqReason.fork"
	msgMQReasonDraft             msgID = "mqReason.draft"
	msgMQReasonNewCommit         msgID = "mqReason.newCommit"
	msgMQReasonUpdateStalled     msgID = "mqReason.updateStalled"
	msgMQReasonUpdateUnconfirmed msgID = "mqReason.updateUnconfirmed"
	msgMQReasonUpdateTimeout     msgID = "mqReason.updateTimeout"
	msgMQReasonChecksFailed      msgID = "mqReason.checksFailed"
	msgMQReasonNoChecks          msgID = "mqReason.noChecks"
	msgMQReasonMissing           msgID = "mqReason.missingRequired"
	msgMQReasonRequiredFailed    msgID = "mqReason.requiredFailed"
	msgMQReasonBranch            msgID = "mqReason.branchNotAllowed"
	msgMQReasonUpdateRejected    msgID = "mqReason.updateRejected"
	msgMQReasonMergeRejected     msgID = "mqReason.mergeRejected"
	msgMQReasonConflictAuthor    msgID = "mqReason.conflictAuthor"
	msgMQReasonConflictDev       msgID = "mqReason.conflictDev"
	msgMQReasonConflictRetarget  msgID = "mqReason.conflictRetargeted"
	msgMQReasonConflictEnded     msgID = "mqReason.conflictEnded"
	msgMQReasonConflictGone      msgID = "mqReason.conflictDevUnavailable"
	msgMQReasonRepoInactive      msgID = "mqReason.repoInactive"
	msgMQReasonMergedOutside     msgID = "mqReason.mergedOutside"

	msgATReasonRepoInactive    msgID = "autoTagReason.repoInactive"
	msgATReasonRetrying        msgID = "autoTagReason.retrying"
	msgATReasonConfigError     msgID = "autoTagReason.configError"
	msgATReasonNoTagConfig     msgID = "autoTagReason.noTagConfig"
	msgATReasonBranchUnknown   msgID = "autoTagReason.branchUnknown"
	msgATReasonNotTagBranch    msgID = "autoTagReason.notTagBranch"
	msgATReasonNotDefault      msgID = "autoTagReason.notDefaultBranch"
	msgATReasonSkipLabel       msgID = "autoTagReason.skipLabel"
	msgATReasonDesignPR        msgID = "autoTagReason.designPR"
	msgATReasonOriginChecking  msgID = "autoTagReason.originChecking"
	msgATReasonOriginUnconfirm msgID = "autoTagReason.originUnconfirmed"
	msgATReasonOriginPending   msgID = "autoTagReason.originPending"
	msgATReasonOriginHead      msgID = "autoTagReason.originHeadMismatch"
	msgATReasonOriginBase      msgID = "autoTagReason.originBaseMismatch"
	msgATReasonOriginStopped   msgID = "autoTagReason.originStopped"
	msgATReasonOriginNoChange  msgID = "autoTagReason.originNoChange"
	msgATReasonOriginNoRecord  msgID = "autoTagReason.originNoRecord"
	msgATReasonBotSender       msgID = "autoTagReason.botSender"
	msgATReasonUnknownSender   msgID = "autoTagReason.unknownSender"
	msgATReasonBotUnauthorized msgID = "autoTagReason.botUnauthorized"
	msgATReasonAlreadyTagged   msgID = "autoTagReason.alreadyTagged"
	msgATReasonVersionOrder    msgID = "autoTagReason.versionOrder"
	msgATReasonVersionOverflow msgID = "autoTagReason.versionOverflow"
	msgATReasonTagOccupied     msgID = "autoTagReason.tagOccupied"
	msgATReasonTagRejected     msgID = "autoTagReason.tagRejected"
	msgATReasonPermission      msgID = "autoTagReason.permission"
)

// 消息的参数名（同一个参数名在各条消息中的含义相同）。
const (
	argLogin = "login"
	// argCount 是数量：复数形式按它选择。
	argCount = "count"
	// argRef 是错误编号（与服务器日志对应），argReason 是原样显示的失败原因（例如 admin.Service 给出的说明）。
	argRef    = "ref"
	argReason = "reason"
	// argID 是 run、job 的编号；argN 是第几次重跑；argAttempt 是 attempt 的编号。
	argID      = "id"
	argN       = "n"
	argJob     = "job"
	argAttempt = "attempt"
	// argValue 是地址或表单中原样的输入值。
	argValue = "value"
	argRepo  = "repo"
	// argGitRef 是分支、tag 或完整的 ref；argFile 是 workflow 文件名。
	argGitRef = "gitRef"
	argFile   = "file"
	// argLine、argFirst 是 inputs 的行号；argName 是 input 的名称；argText 是原样的一行；argOptions 是可选值（已按语言连接）。
	argLine    = "line"
	argFirst   = "first"
	argName    = "name"
	argText    = "text"
	argOptions = "options"
	argOutput  = "output"
	argPath    = "path"
	// argVersion 是版本号；argCurrent、argLatest、argExpected 是 Runner 的当前版本、查到的最新版本和管理员确认的版本；
	// argDetail 是 Runner 上报的错误原文；argProgram 是程序名（kiln-runner）；argCode 是不认识的原因码。
	argVersion  = "version"
	argCurrent  = "current"
	argLatest   = "latest"
	argExpected = "expected"
	argDetail   = "detail"
	argProgram  = "program"
	argCode     = "code"
	// argPoll、argLease 是时长（秒，按 {name, duration} 显示）：一轮领取的最长时间、租约有效期。argAt 是时间（Unix 秒）。
	argPoll  = "poll"
	argLease = "lease"
	argAt    = "at"
	// argFrom 是被重跑的 AI 请求；argTask、argRequest、argItem、argSet 是 AI 任务、AI 请求、工作项和文档快照集的编号；
	// argDoc 是文档的 ID，argActual 是实际的数量（与记录的 argCount 对照）。
	argFrom    = "from"
	argTask    = "task"
	argRequest = "request"
	argItem    = "item"
	argSet     = "set"
	argDoc     = "doc"
	argActual  = "actual"
	// argLabel 是 GitHub 的 label；argChanged、argCandidates 是同步任务池时变化的和可选的 Issue 数。
	argLabel      = "label"
	argChanged    = "changed"
	argCandidates = "candidates"
	// 合并队列与自动打 tag：argPR 是 PR 号；argBranch 是 PR 的目标分支，argFromBranch 是 AI 开发工作项开始时的目标分支，
	// argRelease 是发版分支，argAllowed 是允许合并的分支（已连接）；argPos 是排在第几个；argDone、argTotal 是已结束的和全部的
	// run 数；argTries 是第几次尝试；argChecks、argRuns 是缺少或没有成功的验证、没有通过的 run（已连接）；argSHA 是短 SHA；
	// argState 是原样的状态值（PR 的可合并状态、工作项的状态）；argClosed、argUnknown 是已关闭的和结果不明的 issue（已连接）；
	// argTag 是 tag 名。
	argPR         = "pr"
	argBranch     = "branch"
	argFromBranch = "fromBranch"
	argRelease    = "release"
	argAllowed    = "allowed"
	argPos        = "pos"
	argDone       = "done"
	argTotal      = "total"
	argTries      = "tries"
	argChecks     = "checks"
	argRuns       = "runs"
	argSHA        = "sha"
	argState      = "state"
	argClosed     = "closed"
	argUnknown    = "unknown"
	argTag        = "tag"
)

// messages 是服务端给出的全部消息（只读）。
var messages = map[msgID]msgSpec{
	msgLoginTitle: {}, msgLoginNoticeExpired: {}, msgLoginNoticeRevoked: {}, msgLoginNoticeLogout: {},
	msgLoginErrAdminOff: {}, msgLoginErrAdminEmpty: {}, msgLoginErrAdminWrong: {},

	msgErrLinkRetry: {}, msgErrLinkBack: {}, msgErrLinkRuns: {}, msgErrLinkLogin: {}, msgErrLinkLoginAsAdmin: {},
	msgErrLinkBackToLogin: {}, msgErrLinkRelogin: {}, msgErrLinkGitHubRelogin: {}, msgErrLinkGitHubRetry: {}, msgErrLinkAccount: {},
	msgErrServerHeading: {}, msgErrServerReasonGet: {}, msgErrServerReasonPost: {},
	msgErrNotFoundHeading: {}, msgErrNotFoundReason: {}, msgErrBadRequestHeading: {},
	msgErrForbiddenHeading: {}, msgErrForbiddenReason: {params: map[string]paramKind{argLogin: paramString}},
	msgErrCSRFHeading: {}, msgErrCSRFReason: {},
	msgErrOAuthOffHeading: {}, msgErrOAuthOffReason: {}, msgErrOAuthBadHeading: {}, msgErrOAuthBadReason: {}, msgErrOAuthBrowserReason: {},
	msgErrOAuthExpHeading: {}, msgErrOAuthExpReason: {params: map[string]paramKind{argCount: paramInt}},
	msgErrOAuthDenyHeading: {}, msgErrOAuthDenyReason: {},
	msgErrOAuthNoAccHeading: {}, msgErrOAuthNoAccReason: {params: map[string]paramKind{argLogin: paramString}},
	msgErrOAuthFailHeading: {}, msgErrOAuthFailReason: {}, msgErrBindBadHeading: {}, msgErrBindBadReason: {},

	msgAccountBound:   {params: map[string]paramKind{argLogin: paramString}, flash: true},
	msgAccountUnbound: {flash: true},

	msgActionInternal:  {params: map[string]paramKind{argRef: paramString}},
	msgAPIUnauthorized: {},

	msgNavOverview: {}, msgNavRuns: {},
	msgSetupReposTitle: {}, msgSetupReposText: {}, msgSetupReposAction: {},
	msgSetupRunnerTitle: {}, msgSetupRunnerText: {}, msgSetupRunnerAction: {},
	msgSetupBindTitle: {}, msgSetupBindText: {}, msgSetupBindAction: {},
	msgSetupTokenTitle: {}, msgSetupTokenText: {}, msgSetupTokenAction: {},
	msgRunStateQueued: {}, msgRunStateActive: {}, msgRunStateSucceeded: {}, msgRunStateFailed: {}, msgRunStateCancelled: {}, msgRunStateSkipped: {},

	msgRunNewTitle: {}, msgRunNewPickRepo: {}, msgRunNewPickWorkflow: {},
	msgRunNewRepoMissing:  {params: map[string]paramKind{argRepo: paramString}},
	msgRunNewWfMissing:    {params: map[string]paramKind{argGitRef: paramString, argFile: paramString}},
	msgRunNewWfNoDispatch: {params: map[string]paramKind{argFile: paramString}},
	msgRunNewRefNotFound:  {params: map[string]paramKind{argGitRef: paramString}},
	msgRunNewInputs:       {params: map[string]paramKind{argCount: paramInt}},
	msgRunNewDispatched:   {flash: true},
	msgInputNoEquals:      {params: map[string]paramKind{argLine: paramInt, argText: paramString}},
	msgInputNoName:        {params: map[string]paramKind{argLine: paramInt}},
	msgInputBadName:       {params: map[string]paramKind{argLine: paramInt, argName: paramString}},
	msgInputDuplicate:     {params: map[string]paramKind{argLine: paramInt, argName: paramString, argFirst: paramInt}},
	msgInputMissing:       {params: map[string]paramKind{argName: paramString}},
	msgInputNotBool:       {params: map[string]paramKind{argName: paramString}},
	msgInputNotChoice:     {params: map[string]paramKind{argName: paramString, argOptions: paramString}},
	msgInputUndeclared:    {params: map[string]paramKind{argName: paramString}},

	msgRunTitle:           {params: map[string]paramKind{argID: paramInt}},
	msgRunTitleFirst:      {params: map[string]paramKind{argID: paramInt}},
	msgRunTitleRerun:      {params: map[string]paramKind{argID: paramInt, argN: paramInt}},
	msgRunNotFound:        {params: map[string]paramKind{argValue: paramString}},
	msgRunActDispatch:     {},
	msgRunActCancel:       {},
	msgRunActRerun:        {params: map[string]paramKind{argN: paramInt}},
	msgRunActRerunFailed:  {params: map[string]paramKind{argN: paramInt}},
	msgRunActRerunAll:     {params: map[string]paramKind{argN: paramInt}},
	msgRunActRerunNoCache: {params: map[string]paramKind{argN: paramInt}},
	msgRunActRerunJob:     {params: map[string]paramKind{argJob: paramInt, argN: paramInt}},
	msgRunCancelled:       {flash: true},
	msgRunCancelFailed:    {params: map[string]paramKind{argReason: paramString}, flash: true},
	msgRunCancelFailedRef: {params: map[string]paramKind{argRef: paramString}, flash: true},
	msgRunRerunCreated:    {params: map[string]paramKind{argN: paramInt}, flash: true},
	msgRunRerunFailed:     {params: map[string]paramKind{argReason: paramString}, flash: true},
	msgRunRerunFailedRef:  {params: map[string]paramKind{argRef: paramString}, flash: true},

	msgJobNotFound:          {params: map[string]paramKind{argValue: paramString}},
	msgJobRerunNotFound:     {params: map[string]paramKind{argID: paramInt}},
	msgJobAttemptPublish:    {},
	msgJobAttemptInvalid:    {params: map[string]paramKind{argValue: paramString}},
	msgJobAttemptMissing:    {params: map[string]paramKind{argAttempt: paramInt}},
	msgJobOutputGone:        {params: map[string]paramKind{argOutput: paramString, argPath: paramString}, flash: true},
	msgJobLogPurged:         {},
	msgJobLogNoOutput:       {},
	msgJobLogPublishPending: {},
	msgJobLogReused:         {params: map[string]paramKind{argJob: paramInt}},
	msgJobLogCached:         {params: map[string]paramKind{argJob: paramInt}},
	msgJobLogSkipped:        {},
	msgJobLogQueued:         {},
	msgJobLogNotRun:         {},
	msgJobLogNone:           {},

	msgLogBadAttempt: {}, msgLogBadOffset: {},
	msgLogJobNotFound:         {params: map[string]paramKind{argID: paramInt}},
	msgLogJobAttemptNotFound:  {params: map[string]paramKind{argID: paramInt, argAttempt: paramInt}},
	msgLogTaskNotFound:        {params: map[string]paramKind{argID: paramInt}},
	msgLogTaskAttemptNotFound: {params: map[string]paramKind{argID: paramInt, argAttempt: paramInt}},
	msgLogServer:              {params: map[string]paramKind{argRef: paramString}},

	msgRunnersTitle: {}, msgRunnerNewTitle: {}, msgAIRunnersTitle: {},
	msgRunnerTitle:        {params: map[string]paramKind{argName: paramString}},
	msgRunnerCacheTitle:   {params: map[string]paramKind{argName: paramString}},
	msgAIRunnerTitle:      {params: map[string]paramKind{argName: paramString}},
	msgRunnerNotFound:     {params: map[string]paramKind{argName: paramString}},
	msgAIRunnerNotFound:   {params: map[string]paramKind{argName: paramString}},
	msgRunnerOpUnknown:    {},
	msgRunnerFilterOnline: {}, msgRunnerFilterAccepting: {}, msgRunnerFilterPaused: {},
	msgRunnerFilterBusy: {}, msgRunnerFilterOffline: {}, msgRunnerFilterRevoked: {},

	msgRunnerOpNotFound:       {params: map[string]paramKind{argName: paramString}, flash: true},
	msgRunnerOpNotRevoked:     {params: map[string]paramKind{argName: paramString}, flash: true},
	msgRunnerOpRevokedAlready: {params: map[string]paramKind{argName: paramString}, flash: true},
	msgRunnerOpFailed:         {params: map[string]paramKind{argName: paramString, argReason: paramString}, flash: true},
	msgRunnerOpFailedRef:      {params: map[string]paramKind{argName: paramString, argRef: paramString}, flash: true},
	msgRunnerOpDrained:        {params: map[string]paramKind{argName: paramString, argPoll: paramInt}, flash: true},
	msgRunnerOpDrainedBusy:    {params: map[string]paramKind{argName: paramString, argPoll: paramInt, argCount: paramInt}, flash: true},
	msgRunnerOpUndrained:      {params: map[string]paramKind{argName: paramString}, flash: true},
	msgRunnerOpUndrainedOff:   {params: map[string]paramKind{argName: paramString}, flash: true},
	msgRunnerOpDeleted:        {params: map[string]paramKind{argName: paramString}, flash: true},
	msgRunnerOpRevoked:        {params: map[string]paramKind{argName: paramString}, flash: true},
	msgRunnerOpRevokedBusy:    {params: map[string]paramKind{argName: paramString, argCount: paramInt, argLease: paramInt}, flash: true},
	msgRunnerOpRevokedBusyAI:  {params: map[string]paramKind{argName: paramString, argCount: paramInt, argLease: paramInt}, flash: true},

	msgRunnerNewErrKind: {}, msgRunnerNewErrPool: {}, msgRunnerNewErrName: {},

	msgUpgradeOpUnknown:       {},
	msgUpgradeRequested:       {params: map[string]paramKind{argName: paramString, argVersion: paramString}, flash: true},
	msgUpgradeRequestedOff:    {params: map[string]paramKind{argName: paramString, argVersion: paramString}, flash: true},
	msgUpgradeAlready:         {params: map[string]paramKind{argName: paramString, argVersion: paramString}, flash: true},
	msgUpgradeCancelled:       {params: map[string]paramKind{argName: paramString, argVersion: paramString}, flash: true},
	msgUpgradeCheckRequested:  {params: map[string]paramKind{argName: paramString, argPoll: paramInt}, flash: true},
	msgUpgradeStartFailed:     {params: map[string]paramKind{argReason: paramString}, flash: true},
	msgUpgradeStartFailedRef:  {params: map[string]paramKind{argRef: paramString}, flash: true},
	msgUpgradeCancelFailed:    {params: map[string]paramKind{argReason: paramString}, flash: true},
	msgUpgradeCancelFailedRef: {params: map[string]paramKind{argRef: paramString}, flash: true},
	msgUpgradeCheckFailed:     {params: map[string]paramKind{argReason: paramString}, flash: true},
	msgUpgradeCheckFailedRef:  {params: map[string]paramKind{argRef: paramString}, flash: true},
	msgUpgradeRunnerMissing:   {params: map[string]paramKind{argName: paramString}, flash: true},

	// 不能升级的原因：页面数据中显示，也作为升级操作被拒绝后的提示（flash）
	msgUpgradeReasonAINode:        {flash: true},
	msgUpgradeReasonRevoked:       {flash: true},
	msgUpgradeReasonUnsupported:   {params: map[string]paramKind{argProgram: paramString}, flash: true},
	msgUpgradeReasonNoLauncher:    {params: map[string]paramKind{argProgram: paramString}, flash: true},
	msgUpgradeReasonDisabled:      {flash: true},
	msgUpgradeReasonCheckFailed:   {params: map[string]paramKind{argDetail: paramString}, flash: true},
	msgUpgradeReasonNotChecked:    {flash: true},
	msgUpgradeReasonUpToDate:      {flash: true},
	msgUpgradeReasonNewer:         {params: map[string]paramKind{argCurrent: paramString, argLatest: paramString}, flash: true},
	msgUpgradeReasonUpgrading:     {params: map[string]paramKind{argVersion: paramString}, flash: true},
	msgUpgradeReasonLatestChanged: {params: map[string]paramKind{argLatest: paramString, argExpected: paramString}, flash: true},
	msgUpgradeReasonNoPending:     {flash: true},
	msgUpgradeReasonSwitching:     {params: map[string]paramKind{argVersion: paramString}, flash: true},
	msgUpgradeReasonUnknown:       {params: map[string]paramKind{argCode: paramString}, flash: true},

	msgCacheFormInvalid:    {flash: true},
	msgCachePurgeRequested: {params: map[string]paramKind{argID: paramInt, argName: paramString}, flash: true},
	msgCachePurgeFailed:    {params: map[string]paramKind{argReason: paramString}, flash: true},
	msgCachePurgeFailedRef: {params: map[string]paramKind{argRef: paramString}, flash: true},
	msgCacheRepoNotFound:   {params: map[string]paramKind{argRepo: paramString}},
	msgCacheNoteNoReport:   {},
	msgCacheNoteReport:     {params: map[string]paramKind{argAt: paramInt}},
	msgCacheNoteBusy:       {params: map[string]paramKind{argName: paramString, argCount: paramInt}},
	msgCacheNoteSharedKept: {},
	msgCacheNoteUnknown:    {params: map[string]paramKind{argCode: paramString}},

	msgAIRequestsTitle: {}, msgAITypeReview: {}, msgAITypeDevelop: {},
	msgAIReqStateQueued: {}, msgAIReqStateActive: {}, msgAIReqStateSucceeded: {}, msgAIReqStateFailed: {},
	msgAIReqStateCancelled: {}, msgAIReqStateSkipped: {},
	msgAIRequestTitleReview: {params: map[string]paramKind{argID: paramInt}},
	msgAIRequestTitleDev:    {params: map[string]paramKind{argID: paramInt}},
	msgAIRequestCancelled:   {flash: true},
	msgAIRequestRerun:       {params: map[string]paramKind{argID: paramInt, argFrom: paramInt}, flash: true},
	msgAITaskTitleReview:    {params: map[string]paramKind{argName: paramString, argID: paramInt}},
	msgAITaskTitleDev:       {params: map[string]paramKind{argName: paramString, argID: paramInt}},
	msgAITaskNotFound:       {params: map[string]paramKind{argValue: paramString}},
	msgAITaskRerunNotFound:  {params: map[string]paramKind{argID: paramInt}},
	msgAITaskAttemptInvalid: {params: map[string]paramKind{argValue: paramString}},
	msgAITaskAttemptMissing: {params: map[string]paramKind{argAttempt: paramInt}},
	msgAITaskLogPurged:      {}, msgAITaskLogNoOutput: {}, msgAITaskLogSkipped: {}, msgAITaskLogNotRun: {},
	msgAITaskLogReused: {params: map[string]paramKind{argTask: paramInt}},

	msgAIDevTitle:          {},
	msgAIDevTitleRepo:      {params: map[string]paramKind{argRepo: paramString}},
	msgAIDevRepoNotFound:   {params: map[string]paramKind{argRepo: paramString}},
	msgAIDevItemTitle:      {params: map[string]paramKind{argID: paramInt, argRepo: paramString}},
	msgAIDevItemNotFound:   {params: map[string]paramKind{argID: paramInt}},
	msgAIDevSyncNoRepo:     {flash: true},
	msgAIDevSyncFailed:     {params: map[string]paramKind{argRepo: paramString, argReason: paramString}, flash: true},
	msgAIDevSyncFailedRef:  {params: map[string]paramKind{argRepo: paramString, argRef: paramString}, flash: true},
	msgAIDevSynced:         {params: map[string]paramKind{argRepo: paramString, argCount: paramInt, argLabel: paramString, argChanged: paramInt, argCandidates: paramInt}, flash: true},
	msgAIDevSyncedPartial:  {params: map[string]paramKind{argRepo: paramString, argCount: paramInt, argLabel: paramString, argChanged: paramInt, argCandidates: paramInt}, flash: true},
	msgAIDevPauseNoRepo:    {flash: true},
	msgAIDevResumeNoRepo:   {flash: true},
	msgAIDevPauseFailed:    {params: map[string]paramKind{argRepo: paramString, argReason: paramString}, flash: true},
	msgAIDevPauseFailedRef: {params: map[string]paramKind{argRepo: paramString, argRef: paramString}, flash: true},
	msgAIDevResumeFailed:   {params: map[string]paramKind{argRepo: paramString, argReason: paramString}, flash: true},
	msgAIDevResumeFailRef:  {params: map[string]paramKind{argRepo: paramString, argRef: paramString}, flash: true},
	msgAIDevPaused:         {params: map[string]paramKind{argRepo: paramString}, flash: true},
	msgAIDevResumed:        {params: map[string]paramKind{argRepo: paramString}, flash: true},
	msgAIDevStopBadID:      {flash: true},
	msgAIDevRetryBadID:     {flash: true},
	msgAIDevStopFailed:     {params: map[string]paramKind{argID: paramInt, argReason: paramString}, flash: true},
	msgAIDevStopFailedRef:  {params: map[string]paramKind{argID: paramInt, argRef: paramString}, flash: true},
	msgAIDevRetryFailed:    {params: map[string]paramKind{argID: paramInt, argReason: paramString}, flash: true},
	msgAIDevRetryFailedRef: {params: map[string]paramKind{argID: paramInt, argRef: paramString}, flash: true},
	msgAIDevStopped:        {params: map[string]paramKind{argID: paramInt}, flash: true},
	msgAIDevRetried:        {params: map[string]paramKind{argID: paramInt}, flash: true},
	msgAIDevItemMissing:    {params: map[string]paramKind{argID: paramInt}, flash: true},
	msgAIDevRepoMissing:    {params: map[string]paramKind{argRepo: paramString}, flash: true},
	msgAIDevNotRetryable:   {params: map[string]paramKind{argID: paramInt}, flash: true},
	msgAIDevFinished:       {params: map[string]paramKind{argID: paramInt}, flash: true},

	msgIntegrityCount:     {params: map[string]paramKind{argCount: paramInt, argActual: paramInt}},
	msgIntegrityDocDigest: {params: map[string]paramKind{argDoc: paramString}},
	msgIntegrityManifest:  {}, msgIntegrityMismatch: {},

	msgIssueSetMissing:   {params: map[string]paramKind{argSet: paramInt}},
	msgIssueSetOtherRepo: {params: map[string]paramKind{argSet: paramInt}},
	msgIssueSetDigest:    {}, msgIssueReqSetDigest: {}, msgIssueReqItemSnapshot: {}, msgIssueReqItemDigest: {},
	msgIssueReqSourceUnknown: {}, msgIssueBasisDigest: {},
	msgIssueTaskItemMissing:   {params: map[string]paramKind{argTask: paramInt, argItem: paramInt}},
	msgIssueTaskItemOtherRepo: {params: map[string]paramKind{argTask: paramInt, argItem: paramInt}},
	msgIssueTaskItemSnapshot:  {params: map[string]paramKind{argTask: paramInt}},
	msgIssueTaskDevRequest:    {params: map[string]paramKind{argTask: paramInt, argRequest: paramInt}},
	msgIssueTaskNoWorkItem:    {params: map[string]paramKind{argTask: paramInt}},
	msgIssueTaskClaim:         {params: map[string]paramKind{argTask: paramInt}},
	msgIssueTaskSpec:          {params: map[string]paramKind{argTask: paramInt}},
	msgIssueTaskSpecDigest:    {params: map[string]paramKind{argTask: paramInt}},
	msgIssueTaskRefsMismatch:  {params: map[string]paramKind{argTask: paramInt}},
	msgIssueTaskRefsExtra:     {params: map[string]paramKind{argTask: paramInt}},

	msgNoteBasisConflict: {}, msgNoteBasisUnverifiable: {}, msgNoteOtherWorkItem: {}, msgNoteLegacyDigestPinned: {},
	msgNoteLegacyRefsMismatch: {}, msgNoteLegacyNoneOK: {}, msgNoteLegacyNoneRefs: {}, msgNoteClaimedDynamic: {},
	msgNoteLegacyPinned: {}, msgNoteLegacyNoBasis: {}, msgNoteLegacyInsufficient: {}, msgNoteDeclaredNone: {},
	msgNoteUnknown: {}, msgNoteWorkItemNoBasis: {}, msgNoteNoRefs: {}, msgNoteSameEmpty: {}, msgNoteSameBasis: {},
	msgNoteItemEmptyNoDigest: {}, msgNoteItemEmptyDigest: {}, msgNoteItemRefsMismatch: {}, msgNoteItemRefsNoDigest: {},
	msgNoteItemDigestMismatch: {}, msgNoteSetEmptyNoDigest: {}, msgNoteSetEmptyDigest: {}, msgNoteSetRefsMismatch: {},
	msgNoteSetRefsNoDigest: {}, msgNoteSetDigestMismatch: {},

	msgMQTitle:        {},
	msgMQTitleRepo:    {params: map[string]paramKind{argRepo: paramString}},
	msgMQRepoNotFound: {params: map[string]paramKind{argRepo: paramString}},

	msgMQAddNoRepo:        {flash: true},
	msgMQAddBadPR:         {params: map[string]paramKind{argValue: paramString}, flash: true},
	msgMQAddFailed:        {params: map[string]paramKind{argRepo: paramString, argPR: paramInt, argReason: paramString}, flash: true},
	msgMQAddFailedRef:     {params: map[string]paramKind{argRepo: paramString, argPR: paramInt, argRef: paramString}, flash: true},
	msgMQAddNoForge:       {params: map[string]paramKind{argRepo: paramString, argPR: paramInt}, flash: true},
	msgMQAddRepoMissing:   {params: map[string]paramKind{argRepo: paramString}, flash: true},
	msgMQAlreadyQueued:    {params: map[string]paramKind{argRepo: paramString, argPR: paramInt}, flash: true},
	msgMQAdded:            {params: map[string]paramKind{argRepo: paramString, argPR: paramInt, argLabel: paramString}, flash: true},
	msgMQRemoveNoRepo:     {flash: true},
	msgMQRemoveBadPR:      {params: map[string]paramKind{argValue: paramString}, flash: true},
	msgMQRemoveFailed:     {params: map[string]paramKind{argRepo: paramString, argPR: paramInt, argReason: paramString}, flash: true},
	msgMQRemoveFailedRef:  {params: map[string]paramKind{argRepo: paramString, argPR: paramInt, argRef: paramString}, flash: true},
	msgMQRemoveNoForge:    {params: map[string]paramKind{argRepo: paramString, argPR: paramInt}, flash: true},
	msgMQRemoveRepoMiss:   {params: map[string]paramKind{argRepo: paramString}, flash: true},
	msgMQRemoveRequested:  {params: map[string]paramKind{argRepo: paramString, argPR: paramInt, argLabel: paramString}, flash: true},
	msgATRetryMissing:     {flash: true},
	msgATNotRetryable:     {params: map[string]paramKind{argSHA: paramString}, flash: true},
	msgATRetryFailed:      {params: map[string]paramKind{argSHA: paramString, argReason: paramString}, flash: true},
	msgATRetryFailedRef:   {params: map[string]paramKind{argSHA: paramString, argRef: paramString}, flash: true},
	msgATRetryRepoMissing: {params: map[string]paramKind{argRepo: paramString}, flash: true},
	msgATRetried:          {params: map[string]paramKind{argSHA: paramString}, flash: true},

	msgMQStatusWaitPlan:         {params: map[string]paramKind{argBranch: paramString}},
	msgMQStatusConfigError:      {params: map[string]paramKind{argDetail: paramString}},
	msgMQStatusUpdating:         {},
	msgMQStatusWaitRuns:         {params: map[string]paramKind{argDone: paramInt, argTotal: paramInt}},
	msgMQStatusPosition:         {params: map[string]paramKind{argPos: paramInt}},
	msgMQStatusPosBranch:        {params: map[string]paramKind{argBranch: paramString, argPos: paramInt}},
	msgMQStatusRemoving:         {},
	msgMQStatusMerging:          {},
	msgMQStatusUpdateRetry:      {params: map[string]paramKind{argDetail: paramString}},
	msgMQStatusConflictRead:     {params: map[string]paramKind{argDetail: paramString}},
	msgMQStatusMergeability:     {params: map[string]paramKind{argState: paramString}},
	msgMQStatusBaseRead:         {params: map[string]paramKind{argDetail: paramString}},
	msgMQStatusConflictRec:      {params: map[string]paramKind{argBranch: paramString, argSHA: paramString, argItem: paramInt}},
	msgMQStatusClosingIssues:    {},
	msgMQStatusIssuesClosed:     {params: map[string]paramKind{argClosed: paramString}},
	msgMQStatusIssuesUnknown:    {params: map[string]paramKind{argUnknown: paramString}},
	msgMQStatusIssuesBoth:       {params: map[string]paramKind{argClosed: paramString, argUnknown: paramString}},
	msgMQStatusIssuesRetry:      {params: map[string]paramKind{argTries: paramInt, argDetail: paramString}},
	msgMQStatusIssuesRetryDeny:  {params: map[string]paramKind{argTries: paramInt, argDetail: paramString}},
	msgMQStatusIssuesGaveUp:     {params: map[string]paramKind{argCount: paramInt, argDetail: paramString}},
	msgMQStatusIssuesGaveUpDeny: {params: map[string]paramKind{argCount: paramInt, argDetail: paramString}},
	msgMQStatusIssuesInactive:   {},

	msgMQReasonPRClosed: {}, msgMQReasonFork: {}, msgMQReasonDraft: {}, msgMQReasonNoChecks: {},
	msgMQReasonLabelRemoved:      {params: map[string]paramKind{argLabel: paramString}},
	msgMQReasonNewCommit:         {params: map[string]paramKind{argLabel: paramString}},
	msgMQReasonUpdateStalled:     {params: map[string]paramKind{argCount: paramInt, argDetail: paramString}},
	msgMQReasonUpdateUnconfirmed: {params: map[string]paramKind{argCount: paramInt, argState: paramString}},
	msgMQReasonUpdateTimeout:     {params: map[string]paramKind{argCount: paramInt}},
	msgMQReasonChecksFailed:      {params: map[string]paramKind{argRuns: paramString}},
	msgMQReasonMissing:           {params: map[string]paramKind{argChecks: paramString, argPath: paramString}},
	msgMQReasonRequiredFailed:    {params: map[string]paramKind{argChecks: paramString, argPath: paramString}},
	msgMQReasonBranch:            {params: map[string]paramKind{argBranch: paramString, argAllowed: paramString}},
	msgMQReasonUpdateRejected:    {params: map[string]paramKind{argDetail: paramString}},
	msgMQReasonMergeRejected:     {params: map[string]paramKind{argDetail: paramString}},
	msgMQReasonConflictAuthor:    {params: map[string]paramKind{argBranch: paramString, argSHA: paramString}},
	msgMQReasonConflictDev:       {params: map[string]paramKind{argBranch: paramString, argSHA: paramString, argItem: paramInt}},
	msgMQReasonConflictRetarget: {params: map[string]paramKind{
		argBranch: paramString, argSHA: paramString, argItem: paramInt, argFromBranch: paramString,
	}},
	msgMQReasonConflictEnded: {params: map[string]paramKind{
		argBranch: paramString, argSHA: paramString, argItem: paramInt, argState: paramString,
	}},
	msgMQReasonConflictGone:  {params: map[string]paramKind{argBranch: paramString, argSHA: paramString, argItem: paramInt}},
	msgMQReasonRepoInactive:  {},
	msgMQReasonMergedOutside: {},

	msgATReasonRepoInactive: {}, msgATReasonOriginPending: {}, msgATReasonUnknownSender: {}, msgATReasonBotUnauthorized: {},
	msgATReasonAlreadyTagged:   {},
	msgATReasonRetrying:        {params: map[string]paramKind{argDetail: paramString}},
	msgATReasonConfigError:     {params: map[string]paramKind{argDetail: paramString}},
	msgATReasonNoTagConfig:     {params: map[string]paramKind{argPath: paramString}},
	msgATReasonBranchUnknown:   {params: map[string]paramKind{argRelease: paramString}},
	msgATReasonNotTagBranch:    {params: map[string]paramKind{argBranch: paramString, argRelease: paramString}},
	msgATReasonNotDefault:      {params: map[string]paramKind{argBranch: paramString, argRelease: paramString}},
	msgATReasonSkipLabel:       {params: map[string]paramKind{argLabel: paramString}},
	msgATReasonDesignPR:        {params: map[string]paramKind{argItem: paramInt}},
	msgATReasonOriginChecking:  {params: map[string]paramKind{argItem: paramInt}},
	msgATReasonOriginUnconfirm: {params: map[string]paramKind{argItem: paramInt}},
	msgATReasonOriginHead:      {params: map[string]paramKind{argItem: paramInt}},
	msgATReasonOriginBase:      {params: map[string]paramKind{argItem: paramInt}},
	msgATReasonOriginStopped:   {params: map[string]paramKind{argItem: paramInt}},
	msgATReasonOriginNoChange:  {params: map[string]paramKind{argItem: paramInt}},
	msgATReasonOriginNoRecord:  {params: map[string]paramKind{argItem: paramInt}},
	msgATReasonBotSender:       {params: map[string]paramKind{argLogin: paramString}},
	msgATReasonVersionOrder:    {params: map[string]paramKind{argTag: paramString}},
	msgATReasonVersionOverflow: {params: map[string]paramKind{argTag: paramString}},
	msgATReasonTagOccupied:     {params: map[string]paramKind{argTag: paramString, argSHA: paramString}},
	msgATReasonTagRejected:     {params: map[string]paramKind{argTag: paramString, argDetail: paramString}},
	msgATReasonPermission:      {params: map[string]paramKind{argDetail: paramString}},
}

// uiText 是页面数据中的一段文字：消息（id 加参数，前端按当前语言显示），或原样显示的文字（raw）。零值是空串。
// JSON 编码：消息为 {"id": …, "params": {…}}，原样的文字为字符串。
type uiText struct {
	id     msgID
	params map[string]any
	raw    string
}

// msgArg 是消息的一个参数。
type msgArg struct {
	name  string
	value any
}

// strArg、intArg 返回文字、整数参数。
func strArg(name, v string) msgArg       { return msgArg{name, v} }
func intArg(name string, v int64) msgArg { return msgArg{name, v} }

// message 返回消息 id（参数为 args）。
func message(id msgID, args ...msgArg) uiText {
	t := uiText{id: id}
	if len(args) > 0 {
		t.params = make(map[string]any, len(args))
		for _, a := range args {
			t.params[a.name] = a.value
		}
	}
	return t
}

// rawText 返回原样显示的文字（对象名、原始的说明，以及还没有迁移成消息的文案）。
func rawText(s string) uiText { return uiText{raw: s} }

// IsZero 表示没有文字。
func (t uiText) IsZero() bool { return t.id == "" && t.raw == "" }

// textJSON 是消息的 JSON 形式。
type textJSON struct {
	ID     msgID          `json:"id"`
	Params map[string]any `json:"params,omitempty"`
}

func (t uiText) MarshalJSON() ([]byte, error) {
	if t.id == "" {
		return json.Marshal(t.raw)
	}
	return json.Marshal(textJSON{ID: t.id, Params: t.params})
}

// UnmarshalJSON 解码字符串或消息（测试读取页面数据时用；地址中的提示用 parseFlash，另有校验）。
func (t *uiText) UnmarshalJSON(b []byte) error {
	if s := ""; json.Unmarshal(b, &s) == nil {
		*t = rawText(s)
		return nil
	}
	var m textJSON
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*t = uiText{id: m.ID, params: m.Params}
	return nil
}

// 跳转后的提示（?flash=）的上限：地址可以被任何人构造，超出的不显示。
const (
	// maxFlashBytes 是 flash 参数（消息的 JSON）的最大长度。
	maxFlashBytes = 1024
	// maxFlashArgRunes 是 flash 中一个文字参数的最大字符数。
	maxFlashArgRunes = 200
	// maxLegacyFlashBytes 是还没有迁移的整句提示的最大长度（字节，截断显示）。
	maxLegacyFlashBytes = 600
)

// errFlash 表示地址中的提示不合格（不认识的消息、多余或缺少的参数、类型不对）。
var errFlash = errors.New("提示不合格")

// flashValue 返回放进地址的提示（?flash= 的值）：消息的 JSON。只接受登记为 flash 的消息。
func flashValue(t uiText) (string, error) {
	if _, err := checkFlash(t.id, t.params); err != nil {
		return "", err
	}
	b, err := json.Marshal(textJSON{ID: t.id, Params: t.params})
	if err != nil {
		return "", err
	}
	if len(b) > maxFlashBytes {
		return "", fmt.Errorf("%w：%d 字节，超过 %d", errFlash, len(b), maxFlashBytes)
	}
	return string(b), nil
}

// parseFlash 校验地址中的提示（?flash= 的值）并返回要显示的文字：
//   - 消息（JSON）：只接受登记为 flash 的消息，参数名、类型、长度都要符合，否则不显示（返回零值）；
//   - 其他是还没有迁移的整句提示：原样显示，过长时截断（迁移完成后不再产生）。
//
// 地址可以被任何人构造：提示只是纯文字，前端不把参数当作标签或 HTML。
func parseFlash(v string) uiText {
	if v == "" || len(v) > maxFlashBytes || !utf8.ValidString(v) {
		return uiText{}
	}
	// 消息是 JSON 对象；其他的是旧的整句提示
	if !strings.HasPrefix(v, "{") {
		return rawText(shorten.Text(v, maxLegacyFlashBytes))
	}
	var m textJSON
	dec := json.NewDecoder(strings.NewReader(v))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil || dec.More() {
		return uiText{}
	}
	params, err := checkFlash(m.ID, m.Params)
	if err != nil {
		return uiText{}
	}
	return uiText{id: m.ID, params: params}
}

// checkFlash 检查 flash 中的消息和参数（参数名与登记的完全相同、类型和长度符合），返回规范化的参数。
func checkFlash(id msgID, params map[string]any) (map[string]any, error) {
	spec, ok := messages[id]
	if !ok || !spec.flash {
		return nil, fmt.Errorf("%w：%q 不是可以放进地址的消息", errFlash, id)
	}
	if len(params) != len(spec.params) {
		return nil, fmt.Errorf("%w：%q 的参数个数不对", errFlash, id)
	}
	if len(params) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(params))
	for name, v := range params {
		kind, ok := spec.params[name]
		if !ok {
			return nil, fmt.Errorf("%w：%q 没有参数 %q", errFlash, id, name)
		}
		nv, ok := flashArg(kind, v)
		if !ok {
			return nil, fmt.Errorf("%w：%q 的参数 %q 不合格", errFlash, id, name)
		}
		out[name] = nv
	}
	return out, nil
}

// flashArg 检查一个参数的类型和长度。
func flashArg(kind paramKind, v any) (any, bool) {
	switch kind {
	case paramString:
		s, ok := v.(string)
		return s, ok && utf8.RuneCountInString(s) <= maxFlashArgRunes && !strings.ContainsFunc(s, isControl)
	case paramInt:
		switch n := v.(type) {
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		case int64:
			return n, true
		case int:
			return int64(n), true
		case float64:
			return int64(n), n == math.Trunc(n) && math.Abs(n) < 1<<53
		}
	}
	return nil, false
}

// flashArgText 把原样的文字（对象名、路径、失败原因）整理为能放进 flash 的参数：控制字符换成空格，
// 超过 maxFlashArgRunes 个字符时截断并以省略号结尾，避免整条提示因为参数不合格而不显示。
func flashArgText(s string) string {
	s = strings.Map(func(r rune) rune {
		if isControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, shorten.Ellipsis))
	if utf8.RuneCountInString(s) <= maxFlashArgRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxFlashArgRunes-1]) + shorten.Ellipsis
}

// fitFlash 让提示放得进地址：文字参数先按 flashArgText 整理，整条消息的 JSON 超过 maxFlashBytes 时
// 再按字节预算截断文字参数（以省略号结尾），保证长度合格的消息总能带着提示跳转。
// 单个参数的字符上限不能保证整条 JSON 的字节上限：转义（& 编码为 \u0026，6 个字节）、多字节字符和多个参数都会叠加。
// 预算在文字参数之间平分，用不完的份额留给更长的参数；参数名和数字参数不变。
func fitFlash(t uiText) uiText {
	texts := make(map[string]string) // 文字参数（整理后）
	params := make(map[string]any, len(t.params))
	for name, v := range t.params {
		if s, ok := v.(string); ok {
			texts[name] = flashArgText(s)
			v = texts[name]
		}
		params[name] = v
	}
	t.params = params
	if len(texts) == 0 {
		return t
	}
	// 文字参数都换成空串时的长度：消息 id、参数名、数字参数和引号，与文字参数的内容无关
	empty := maps.Clone(params)
	total := 0
	for name, s := range texts {
		empty[name] = ""
		total += jsonStringLen(s)
	}
	b, err := json.Marshal(textJSON{ID: t.id, Params: empty})
	if err != nil {
		return t
	}
	budget := maxFlashBytes - len(b)
	if total <= budget || budget < 0 {
		return t // 放得下；或者连空参数都放不下（由 flashValue 报告）
	}
	// 从短到长分配：放得下的原样保留，余下的预算由更长的参数平分
	names := slices.Collect(maps.Keys(texts))
	slices.SortFunc(names, func(a, b string) int {
		return cmp.Or(cmp.Compare(jsonStringLen(texts[a]), jsonStringLen(texts[b])), cmp.Compare(a, b))
	})
	for i, name := range names {
		s := truncateJSONString(texts[name], budget/(len(names)-i))
		params[name] = s
		budget -= jsonStringLen(s)
	}
	return t
}

// jsonStringLen 是 s 编码为 JSON 字符串后的字节数（不含两边的引号）。
func jsonStringLen(s string) int {
	b, _ := json.Marshal(s) // 字符串总能编码
	return len(b) - len(`""`)
}

// truncateJSONString 截断 s，使它编码为 JSON 字符串后不超过 limit 个字节（不含引号）；截断时以省略号结尾，不切开字符。
func truncateJSONString(s string, limit int) string {
	if jsonStringLen(s) <= limit {
		return s
	}
	limit -= jsonStringLen(shorten.Ellipsis)
	if limit < 0 {
		return ""
	}
	n := 0
	for i, r := range s {
		n += jsonStringLen(string(r))
		if n > limit {
			return s[:i] + shorten.Ellipsis
		}
	}
	return s // 不会到达：编码后超过 limit 的 s 在循环中已经截断
}

// isControl 表示控制字符（换行、制表符等，不应出现在提示的参数中）。
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
