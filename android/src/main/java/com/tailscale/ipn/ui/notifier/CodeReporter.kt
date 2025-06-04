package com.tailscale.ipn.ui.notifier

import android.util.Log
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import org.json.JSONObject
import java.io.IOException
import java.util.concurrent.TimeUnit

/**
 * 类的作用的简要阐述
 *
 * <p>创建时间：2025/6/4/周三</p>
 *
 * @author rwz
 */
class CodeReporter(
    private val timeoutSecond: Long = 60
) {

    private val client by lazy {
        OkHttpClient.Builder()
            .connectTimeout(timeoutSecond, TimeUnit.SECONDS)
            .readTimeout(timeoutSecond, TimeUnit.SECONDS)
            .writeTimeout(timeoutSecond, TimeUnit.SECONDS)
            .build()
    }

    fun fetchJson(request: Request): Result<String?> {
        return kotlin.runCatching {
            val resp = execute(request)
            if (resp.code == 200) {
                resp.body?.string()
            } else {
                throw IOException("HTTP code: ${resp.code}, error: ${resp.message}")
            }
        }
    }

    private fun execute(request: Request): Response {
        return client.newCall(request).execute()
    }

    fun release() {
        client.connectionPool.evictAll()
    }

    fun report(url: String): Result<Boolean> {
        Log.d("CodeReporter", "report url: $url")
        val request = Request.Builder()
            .url(url)
            .get()
            .build()
        return fetchJson(request).mapCatching {
            Log.d("CodeReporter", "report: $it")
            if (it.isNullOrEmpty()) {
                false
            } else {
                JSONObject(it).getBoolean("success")
            }
        }.onFailure {
            it.printStackTrace()
        }
    }
}