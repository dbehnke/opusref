import { ApiError, type ApiEnvelope } from './types'
export { ApiError } from './types'

type RequestOptions = Omit<RequestInit, 'body'> & { body?: unknown; csrf?: string; reauth?: string }

export class ApiClient {
  constructor(private readonly base = '') {}

  async request<T>(path: string, options: RequestOptions = {}): Promise<T> {
    const headers = new Headers(options.headers)
    headers.set('Accept', 'application/json')
    if (options.body !== undefined) headers.set('Content-Type', 'application/json')
    if (options.csrf) headers.set('X-CSRF-Token', options.csrf)
    if (options.reauth) headers.set('X-Reauth-Token', options.reauth)
    const response = await fetch(`${this.base}${path}`, {
      ...options,
      credentials: 'same-origin',
      headers,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
    })
    const payload = await response.json().catch(() => ({ code: 'invalid_response', message: 'The server returned an invalid response.' }))
    if (!response.ok) throw new ApiError(payload.code ?? 'request_failed', payload.message ?? 'The request failed.', response.status)
    const envelope = payload as ApiEnvelope<T>
    if (envelope.api_version !== 1) throw new ApiError('unsupported_api_version', 'The server response version is not supported.', 500)
    return envelope.data
  }
}

export const api = new ApiClient()
