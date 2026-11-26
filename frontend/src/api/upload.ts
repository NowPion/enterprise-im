import request from '@/api/request'
import { normalizeApiError, unwrapApiResponse } from '@/api/conversation'
import type { Asset, DeleteAssetResult, UploadPurpose } from '@/types/upload'

export interface UploadAssetOptions {
  filename?: string
  onProgress?: (percent: number) => void
}

export async function uploadAsset(
  file: File | Blob,
  purpose: UploadPurpose,
  options: UploadAssetOptions = {},
): Promise<Asset> {
  const formData = new FormData()
  formData.append('purpose', purpose)
  if (file instanceof File) {
    formData.append('file', file, options.filename ?? file.name)
  } else {
    formData.append('file', file, options.filename ?? 'upload.bin')
  }

  try {
    const res = await request.post('/uploads', formData, {
      headers: { 'Content-Type': 'multipart/form-data' },
      onUploadProgress: (evt) => {
        if (!options.onProgress || !evt.total || evt.total <= 0) return
        const percent = Math.min(100, Math.round((evt.loaded * 100) / evt.total))
        options.onProgress(percent)
      },
    })
    return unwrapApiResponse<Asset>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function fetchAssetMeta(assetId: string): Promise<Asset> {
  try {
    const res = await request.get(`/uploads/${encodeURIComponent(assetId)}`)
    return unwrapApiResponse<Asset>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export async function deleteAsset(assetId: string): Promise<DeleteAssetResult> {
  try {
    const res = await request.delete(`/uploads/${encodeURIComponent(assetId)}`)
    return unwrapApiResponse<DeleteAssetResult>(res)
  } catch (err) {
    throw normalizeApiError(err)
  }
}

export function normalizeAssetRequestPath(urlOrAssetId: string): string {
  const trimmed = urlOrAssetId.trim()
  if (trimmed.startsWith('/api/v1/')) {
    return trimmed.slice('/api/v1'.length)
  }
  if (trimmed.startsWith('/uploads/')) {
    return trimmed
  }
  return `/uploads/${encodeURIComponent(trimmed)}/download`
}

export async function fetchAssetBlob(
  urlOrAssetId: string,
  options: { variant?: 'thumbnail'; download?: boolean } = {},
): Promise<Blob> {
  const path = normalizeAssetRequestPath(urlOrAssetId)
  const params: Record<string, string> = {}
  if (options.variant) params.variant = options.variant
  if (options.download) params.download = '1'

  try {
    const res = await request.get(path, {
      params: Object.keys(params).length > 0 ? params : undefined,
      responseType: 'blob',
    })
    if (res instanceof Blob) {
      return res
    }
    if (typeof res === 'object' && res !== null && 'data' in res && (res as { data: unknown }).data instanceof Blob) {
      return (res as { data: Blob }).data
    }
    throw new Error('下载响应格式异常')
  } catch (err) {
    throw normalizeApiError(err)
  }
}
