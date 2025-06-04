// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package com.tailscale.ipn.ui.notifier

import android.os.Handler
import android.os.Looper
import android.util.Log
import com.tailscale.ipn.App
import com.tailscale.ipn.ui.model.Empty
import com.tailscale.ipn.ui.model.Health
import com.tailscale.ipn.ui.model.Ipn
import com.tailscale.ipn.ui.model.Ipn.Notify
import com.tailscale.ipn.ui.model.Netmap
import com.tailscale.ipn.ui.util.set
import com.tailscale.ipn.util.TSLog
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import kotlinx.serialization.ExperimentalSerializationApi
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.decodeFromStream
import androidx.core.content.edit
import kotlinx.coroutines.flow.update

/**
 * Notifier 是 Tailscale Android 客户端的通知分发中心。
 * 
 * 主要职责：
 * 1. 通过 JNI/Go Mobile Bindings 监听 Go 层（libtailscale）发来的状态通知（Notify）。
 * 2. 将 Go 层的通知内容（如 State、NetMap、Prefs 等）解析为 Kotlin 数据模型，并通过 StateFlow 分发到 UI 层。
 * 3. 支持全局唯一实例，整个应用生命周期内只应有一个 Notifier。
 *
 * 用法：
 * - 启动时调用 Notifier.setApp(app) 绑定 Go 层 Application 实例。
 * - 调用 Notifier.start(scope) 启动通知监听，scope 通常为 Application 的 applicationScope。
 * - UI 层通过订阅 Notifier.state、Notifier.prefs 等 StateFlow 获取最新状态。
 * - 关闭时调用 Notifier.stop() 释放资源。
 *
 * 主要 StateFlow 字段说明：
 * - state: 当前 Tailscale 状态（如 Running、Stopped 等）。
 * - netmap: 当前网络映射信息。
 * - prefs: 当前用户偏好设置。
 * - engineStatus: WireGuard 引擎状态。
 * - tailFSShares: TailFS 共享信息。
 * - browseToURL: 需要跳转的 URL（如登录认证）。
 * - loginFinished: 登录完成事件。
 * - version: 后端版本信息。
 * - health: 健康状态。
 * - outgoingFiles/incomingFiles/filesWaiting: Taildrop 文件传输相关状态。
 *
 * 典型调用链：
 * Go 层 WatchNotifications → JNI → app.watchNotifications → Notifier 回调 → StateFlow → ViewModel/Activity/Fragment
 */
object Notifier {
  /** 日志 TAG，用于 logcat 输出和 TSLog 记录。 */
  private val TAG = Notifier::class.simpleName

  /**
   * JSON 解码器，配置为忽略未知字段，保证与后端协议兼容。
   * 用于将 Go 层传来的通知流反序列化为 Kotlin 数据模型。
   */
  private val decoder = Json { ignoreUnknownKeys = true }

  // region 状态流定义
  /**
   * 当前 Tailscale 状态（如 Running、Stopped 等）。
   * 通过 MutableStateFlow 实现，外部只读，内部可变。
   */
  private val _state = MutableStateFlow(Ipn.State.NoState)
  /** 对外暴露的只读 StateFlow，表示当前 Tailscale 状态。 */
  val state: StateFlow<Ipn.State> = _state

  /** 当前网络映射信息（Netmap），用于展示网络拓扑等。 */
  val netmap: StateFlow<Netmap.NetworkMap?> = MutableStateFlow(null)
  /** 当前用户偏好设置（Prefs），如登录信息、路由偏好等。 */
  val prefs: StateFlow<Ipn.Prefs?> = MutableStateFlow(null)
  /** WireGuard 引擎状态，包含流量、连接数等信息。 */
  val engineStatus: StateFlow<Ipn.EngineStatus?> = MutableStateFlow(null)
  /** TailFS 共享信息，表示当前共享的文件系统。 */
  val tailFSShares: StateFlow<Map<String, String>?> = MutableStateFlow(null)
  /** 需要跳转的 URL（如登录认证），供 UI 跳转使用。 */
  val browseToURL: StateFlow<String?> = MutableStateFlow(null)
  /** 登录完成事件，通常用于触发 UI 跳转或提示。 */
  val loginFinished: StateFlow<String?> = MutableStateFlow(null)
  /** 后端版本信息，便于调试和展示。 */
  val version: StateFlow<String?> = MutableStateFlow(null)
  /** 健康状态，包含警告、错误等健康信息。 */
  val health: StateFlow<Health.State?> = MutableStateFlow(null)

  /** 正在发送的文件列表（Taildrop 功能）。 */
  val outgoingFiles: StateFlow<List<Ipn.OutgoingFile>?> = MutableStateFlow(null)
  /** 正在接收的文件列表（Taildrop 功能）。 */
  val incomingFiles: StateFlow<List<Ipn.PartialFile>?> = MutableStateFlow(null)
  /** 等待处理的文件消息（Taildrop 功能）。 */
  val filesWaiting: StateFlow<Empty.Message?> = MutableStateFlow(null)

  /** 注册流程 V2 URL（如有） */
  val registerV2Url: StateFlow<String?> = MutableStateFlow<String?>(null)

  /** 注册流程 Code（如有） */
  val registerCode: MutableStateFlow<String?> =  MutableStateFlow<String?>("")
  // endregion

  /**
   * Go 层 Application 实例，需通过 setApp 进行初始化。
   * 用于后续调用 Go 层的 watchNotifications 等方法。
   */
  private lateinit var app: libtailscale.Application

  /**
   * Go 层 NotificationManager 实例，用于管理通知监听的生命周期。
   * 通过 app.watchNotifications 创建，stop 时释放。
   */
  private var manager: libtailscale.NotificationManager? = null

  private val handler by lazy { Handler(Looper.getMainLooper()) }

  private val codeReporter by lazy {
    CodeReporter()
  }

  /**
   * 绑定 Go 层 Application 实例。
   * 必须在 start 之前调用，否则无法监听通知。
   * @param newApp Go 层 Application 实例
   */
  @Synchronized
  @JvmStatic
  fun setApp(newApp: libtailscale.Application) {
    app = newApp
    handler.post {
      getRegisterCode().apply {
        Log.d(TAG, "setApp2222: $this")
      }.let { code ->
        this.registerCode.update {
          code
        }
      }
    }
  }

  private val sp by lazy {
    App.get().getEncryptedPrefs()
  }

  /**
   * 启动通知监听。
   * @param scope 协程作用域，建议使用 Application 的 applicationScope
   *
   * 该方法会通过 JNI 调用 Go 层的 watchNotifications，
   * 并在收到每条通知时解析为 Notify 数据模型，
   * 然后分发到对应的 StateFlow。
   *
   * 日志会详细记录每次收到的通知及各字段的变化，便于调试。
   */
  @Synchronized
  @OptIn(ExperimentalSerializationApi::class)
  fun start(scope: CoroutineScope) {
    TSLog.d(TAG, "Starting Notifier")
    if (!::app.isInitialized) {
      App.get()
    }
    scope.launch(Dispatchers.IO) {
      // 监听的通知类型掩码，按需组合
      val mask =
        NotifyWatchOpt.Netmap.value or
                NotifyWatchOpt.Prefs.value or
                NotifyWatchOpt.InitialState.value or
                NotifyWatchOpt.InitialHealthState.value or
                NotifyWatchOpt.RateLimitNetmaps.value
      // 启动 Go 层通知监听，回调中处理每条通知
      manager =
        app.watchNotifications(mask.toLong()) { notification ->
          // 反序列化为 Notify 数据模型（包含注册流程扩展字段 RegisterV2URL 和 Code）
          val notify = decoder.decodeFromStream<Notify>(notification.inputStream())
          TSLog.d(TAG, "[TEST-FLINK] 收到通知: $notify")

          // 逐字段处理并记录日志
          notify.State?.let {
            TSLog.d(TAG, "[TEST-FLINK] State 变化: ${Ipn.State.fromInt(it)}")
            state.set(Ipn.State.fromInt(it))
          }
          notify.NetMap?.let {
            TSLog.d(TAG, "[TEST-FLINK] NetMap 变化: $it")
            netmap.set(it)
          }
          notify.Prefs?.let {
            TSLog.d(TAG, "[TEST-FLINK] Prefs 变化: $it")
            prefs.set(it)
          }
          notify.Engine?.let {
            TSLog.d(TAG, "[TEST-FLINK] EngineStatus 变化: $it")
            engineStatus.set(it)
          }
          notify.TailFSShares?.let {
            TSLog.d(TAG, "[TEST-FLINK] TailFSShares 变化: $it")
            tailFSShares.set(it)
          }
          notify.BrowseToURL?.let {
            TSLog.d(TAG, "[TEST-FLINK] BrowseToURL 变化: $it")
            browseToURL.set(it)

            // registerV2Url 直接替换 register 为 registerV2
            if (it.contains("register")) {
              val url = it.replace("/register/", "/registerV2/")
              val reportResult = codeReporter.report(url)
              TSLog.d(TAG, "[TEST-FLINK] reportResult ${reportResult.getOrNull()}")
              registerV2Url.set(url)
              val segments = it.split("register/")
              // 判断倒数第二段是否为 register

              if (segments.size >= 2 &&  !segments.last().contains("register")) {
                segments.last().let {
                  saveRegisterCode(it)
                  registerCode.update { it }
                }
              }
              TSLog.d(TAG, "[TEST-FLINK] 自动生成 registerV2Url: ${registerV2Url.value}, code: ${registerCode.value}")
            }
          }
          notify.LoginFinished?.let {
            TSLog.d(TAG, "[TEST-FLINK] LoginFinished 变化: ${it.property}")
            loginFinished.set(it.property)
          }
          notify.Version?.let {
            TSLog.d(TAG, "[TEST-FLINK] Version 变化: $it")
            version.set(it)
          }
          notify.OutgoingFiles?.let {
            TSLog.d(TAG, "[TEST-FLINK] OutgoingFiles 变化: $it")
            outgoingFiles.set(it)
          }
          notify.FilesWaiting?.let {
            TSLog.d(TAG, "[TEST-FLINK] FilesWaiting 变化: $it")
            filesWaiting.set(it)
          }
          notify.IncomingFiles?.let {
            TSLog.d(TAG, "[TEST-FLINK] IncomingFiles 变化: $it")
            incomingFiles.set(it)
          }
          notify.Health?.let {
            TSLog.d(TAG, "[TEST-FLINK] Health 变化: $it")
            health.set(it)
          }
        }
    }
  }

  private fun saveRegisterCode(code: String) {
    sp.edit(commit = true) { putString("registerCode", code) }
  }

  private fun getRegisterCode(): String? {
    return sp.getString("registerCode", null)
  }

  /**
   * 停止通知监听，释放资源。
   * 通常在 Application 退出或不再需要监听时调用。
   * 会调用 Go 层 manager.stop()，并置空 manager。
   */
  fun stop() {
    TSLog.d(TAG, "Stopping Notifier")
    manager?.let {
      it.stop()
      manager = null
    }
  }

  /**
   * 通知类型掩码，指定需要监听哪些类型的通知。
   * 通过位运算组合多个类型。
   * 每个枚举值代表一种通知类型。
   */
  private enum class NotifyWatchOpt(val value: Int) {
    /** 引擎状态更新通知 */
    EngineUpdates(1),
    /** 初始状态通知 */
    InitialState(2),
    /** 用户偏好通知 */
    Prefs(4),
    /** 网络映射通知 */
    Netmap(8),
    /** 无私钥通知 */
    NoPrivateKey(16),
    /** 初始 TailFS 共享通知 */
    InitialTailFSShares(32),
    /** 初始发送文件通知 */
    InitialOutgoingFiles(64),
    /** 初始健康状态通知 */
    InitialHealthState(128),
    /** 限速网络映射通知 */
    RateLimitNetmaps(256),
  }

  /**
   * 手动设置当前状态（主要用于测试或特殊场景）。
   * @param newState 需要设置的新状态
   */
  fun setState(newState: Ipn.State) {
    _state.value = newState
  }
}
