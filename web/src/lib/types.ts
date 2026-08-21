export type Role = 'admin' | 'user'
export type Health = 'ok' | 'degraded' | 'unavailable'

export interface SessionInfo {
  authenticated: boolean
  username?: string
  role?: Role
  source_callsign?: string
  csrf_token?: string
  passkey_available: boolean
  forced_password_change?: boolean
}

export interface PublicStatus {
  health: Health
  ready: boolean
  reflector: { id: string; display_name: string }
  client_count: number
  floor: { active: boolean; source_callsign?: string; started_at?: string; remaining_seconds?: number }
  recording: { available: boolean; quota_full: boolean }
  server_time: string
}

export interface Recording {
  id: string
  source_callsign: string
  started_at: string
  duration_ms: number
  status: 'complete' | 'partial'
  end_reason: string
}

export interface Account {
  id: string
  username: string
  role: Role
  source_callsign?: string
  disabled: boolean
  forced_password_change: boolean
}

export interface Passkey { id: string; name: string; created_at: string; last_used_at?: string }
export interface UserSession { id: string; created_at: string; last_active_at: string; current: boolean }
export interface AuditEvent { id: string; occurred_at: string; action: string; result: string; actor?: string }
export interface Page<T> { items: T[]; next_cursor?: string }

export interface ApiEnvelope<T> { api_version: 1; data: T }

export class ApiError extends Error {
  constructor(public readonly code: string, message: string, public readonly status: number) { super(message) }
}
