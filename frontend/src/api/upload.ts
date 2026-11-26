import request, { apiDelete, apiGet, apiPost } from '@/api/request'
import { ApiError } from '@/types/common'
import type { Asset, DeleteAssetResult, UploadPurpose } from '@/types/upload'

export interface UploadAssetOptions {
  filename?: string
  onProgress?: (percent: number) => void
}

export interface AssetRequestTarget {
  url: string
  useApiBase: boolean
}

export function uploadAsset(
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

  return apiPost<Asset>('/uploads', formData, {
    headers: { 'Content-Type': 'multipart/form-data' },
    onUploadProgress: (evt) => {
      if (!options.onProgress || !evt.total || evt.total <= 0) return
      const percent = Math.min(100, Math.round((evt.loaded * 100) / evt.total))
      options.onProgress(percent)
    },
  })
}

export function fetchAssetMeta(assetId: string): Promise<Asset> {
  return apiGet<Asset>(`/uploads/${encodeURIComponent(assetId)}`)
}

export function deleteAsset(assetId: string): Promise<DeleteAssetResult> {
  return apiDelete<DeleteAssetResult>(`/uploads/${encodeURIComponent(assetId)}`)
}

/**
 * Resolves asset paths per docs/api.md:
 * - `/api/v1/...` or bare `asset_id` -> `/uploads/...` relative to `request` (`/api/v1`)
 * - root `/uploads/...` -> `/uploads/...` against site root (`baseURL: ''`)
 */
export function resolveAssetRequestTarget(urlOrAssetId: string): AssetRequestTarget {
  const trimmed = urlOrAssetId.trim()
  if (trimmed.startsWith('/api/v1/')) {
    return {
      url: trimmed.slice('/api/v1'.length),
      useApiBase: true,
    }
  }
  if (trimmed.startsWith('/uploads/')) {
    return {
      url: trimmed,
      useApiBase: false,
    }
  }
  return {
    url: `/uploads/${encodeURIComponent(trimmed)}/download`,
    useApiBase: true,
  }
}

export function normalizeAssetRequestPath(urlOrAssetId: string): string {
  return resolveAssetRequestTarget(urlOrAssetId).url
}

export async function fetchAssetBlob(
  urlOrAssetId: string,
  options: { variant?: 'thumbnail'; download?: boolean } = {},
): Promise<Blob> {
  const target = resolveAssetRequestTarget(urlOrAssetId)
  const params: Record<string, string> = {}
  if (options.variant) params.variant = options.variant
  if (options.download) params.download = '1'

  const res = await request.get<Blob>(target.url, {
    baseURL: target.useApiBase ? undefined : '',
    params: Object.keys(params).length > 0 ? params : undefined,
    responseType: 'blob',
  })
  const raw = res as unknown
  if (raw instanceof Blob) {
    return raw
  }
  if (typeof raw === 'object' && raw !== null && 'data' in raw && (raw as { data: unknown }).data instanceof Blob) {
    return (raw as { data: Blob }).data
  }
  throw new ApiError(-1, '下载响应格式异常')
}
