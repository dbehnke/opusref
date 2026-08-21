import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiClient, ApiError } from '../src/lib/api'
afterEach(() => vi.restoreAllMocks())
describe('API client', () => {
  it('sends credentials and CSRF in headers', async () => { const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({ api_version: 1, data: { ok: true } }), { status: 200, headers: { 'Content-Type': 'application/json' } })); await new ApiClient().request('/test', { method: 'POST', csrf: 'token', body: {} }); expect(fetchMock).toHaveBeenCalledWith('/test', expect.objectContaining({ credentials: 'same-origin', headers: expect.any(Headers) })); const headers = fetchMock.mock.calls[0]?.[1]?.headers as Headers; expect(headers.get('X-CSRF-Token')).toBe('token') })
  it('rejects an unsupported API response', async () => { vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({ api_version: 2, data: {} }), { status: 200 })); await expect(new ApiClient().request('/test')).rejects.toBeInstanceOf(ApiError) })
})
