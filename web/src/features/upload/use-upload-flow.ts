import { useState } from 'react'
import { client } from '../../api/client'
import { apiErrorMessage } from '../../lib/api-error'
import type { components } from '../../api/types'

export const VIDEO_MAX_SIZE = 536_870_912 // 512MB（契约上限）
export const VIDEO_MIN_SIZE = 1_048_576 // 1MB

export type UploadStage = 'idle' | 'requesting' | 'uploading' | 'creating' | 'done' | 'error'

// 与 openapi.yaml UploadURLRequest.content_type 枚举精确对齐
type VideoContentType = Extract<
  components['schemas']['UploadURLRequest']['content_type'],
  string
>

/** 文件扩展名 → 契约 content_type（与 openapi.yaml 枚举对齐） */
export function contentTypeForFile(file: File): VideoContentType | null {
  const ext = file.name.split('.').pop()?.toLowerCase()
  switch (ext) {
    case 'mp4':
      return 'video/mp4'
    case 'webm':
      return 'video/webm'
    case 'mov':
      return 'video/quicktime'
    default:
      return null
  }
}

/** 本地读视频时长（秒）——duration_seconds 是 API 必填，只能客户端读 */
export function readVideoDuration(file: File): Promise<number> {
  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(file)
    const video = document.createElement('video')
    video.preload = 'metadata'
    video.muted = true
    video.onloadedmetadata = () => {
      URL.revokeObjectURL(url)
      resolve(Number.isFinite(video.duration) ? Math.round(video.duration) : 0)
    }
    video.onerror = () => {
      URL.revokeObjectURL(url)
      reject(new Error('无法读取视频信息，请确认文件是有效的视频'))
    }
    video.src = url
  })
}

/**
 * 预签名 PUT 上传（带进度）。
 * 关键：Content-Type 必须与票证声明的类型一致——
 * 不设 header 时 MinIO 存 application/x-www-form-urlencoded，
 * 浏览器 ORB 会拦截跨源播放（F2 已验证的真实事故）。
 */
export function putWithProgress(
  uploadUrl: string,
  file: File,
  contentType: string,
  onProgress: (percent: number) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('PUT', uploadUrl)
    xhr.setRequestHeader('Content-Type', contentType)
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) {
        onProgress(Math.round((e.loaded / e.total) * 100))
      }
    }
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        onProgress(100)
        resolve()
      } else {
        // 预签名过期/伪造 → MinIO 返回 4xx（常见 403）
        reject(new Error(`上传被拒绝（HTTP ${xhr.status}），上传地址可能已过期，请重试`))
      }
    }
    xhr.onerror = () => reject(new Error('网络异常，上传中断'))
    xhr.send(file)
  })
}

export interface UploadFlowResult {
  videoId: number
  objectKey: string
}

/**
 * 三步上传状态机：票证 → PUT（进度）→ 建记录。
 * 任一步失败进 error 态；重试从「重新申请票证」开始（旧票证可能已过期）。
 */
export function useUploadFlow() {
  const [stage, setStage] = useState<UploadStage>('idle')
  const [progress, setProgress] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<UploadFlowResult | null>(null)

  async function start(file: File, title: string) {
    setResult(null)
    setError(null)

    // 1. 客户端校验（与契约一致）
    const contentType = contentTypeForFile(file)
    if (!contentType) {
      setStage('error')
      setError('仅支持 mp4 / webm / mov 格式')
      return false
    }
    if (file.size < VIDEO_MIN_SIZE || file.size > VIDEO_MAX_SIZE) {
      setStage('error')
      setError('文件大小需在 1MB - 512MB 之间')
      return false
    }

    try {
      // 2. 读时长
      const duration = await readVideoDuration(file)
      if (duration < 1) {
        setStage('error')
        setError('无法读取视频时长，文件可能已损坏')
        return false
      }

      // 3. 申请票证
      setStage('requesting')
      const ticket = await client.POST('/api/v1/videos/upload-url', {
        body: { filename: file.name, file_size: file.size, content_type: contentType },
      })
      if (ticket.error) throw new Error(apiErrorMessage(ticket.error))
      if (!ticket.data) throw new Error('未获取到上传地址')

      // 4. PUT（带进度）
      setStage('uploading')
      setProgress(0)
      await putWithProgress(ticket.data.upload_url, file, contentType, setProgress)

      // 5. 建记录
      setStage('creating')
      const created = await client.POST('/api/v1/videos', {
        body: {
          title,
          object_key: ticket.data.object_key,
          duration_seconds: duration,
        },
      })
      if (created.error) {
        // 400：票证过期/伪造/文件与票证不符
        throw new Error(apiErrorMessage(created.error, '上传凭证校验失败，请重新上传'))
      }
      if (!created.data) throw new Error('创建视频记录失败')

      setResult({ videoId: created.data.video_id, objectKey: ticket.data.object_key })
      setStage('done')
      return true
    } catch (err) {
      setStage('error')
      setError(err instanceof Error ? err.message : '上传失败')
      return false
    }
  }

  function reset() {
    setStage('idle')
    setProgress(0)
    setError(null)
    setResult(null)
  }

  return { stage, progress, error, result, start, reset }
}
