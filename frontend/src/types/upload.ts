export type UploadPurpose = 'image' | 'file' | 'voice' | 'avatar' | 'group_avatar'

export interface Asset {
  asset_id: string
  purpose: UploadPurpose
  url: string
  download_url: string
  filename: string
  mime: string
  size_bytes: number
  width?: number
  height?: number
  thumbnail_url?: string
  duration_ms?: number
}

export interface DeleteAssetResult {
  asset_id: string
  deleted: boolean
}
