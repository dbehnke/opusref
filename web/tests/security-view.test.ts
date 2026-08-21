import { fireEvent, render, screen, waitFor } from '@testing-library/vue'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import SecurityView from '../src/views/SecurityView.vue'
import { useSessionStore } from '../src/stores/session'

const fixture = vi.hoisted(() => ({ rejectDelete: undefined as ((error: Error) => void) | undefined }))
vi.mock('../src/lib/api', async importOriginal => {
  const actual = await importOriginal<typeof import('../src/lib/api')>()
  return { ...actual, api: { request: vi.fn(async (path: string, options?: { method?: string }) => {
    if (path.includes('/sessions')) return { items: [] }
    if (path.includes('/passkeys?')) return { items: [{ id: 'key-1', name: 'Travel key', created_at: '2026-08-20T12:00:00Z' }] }
    if (path.includes('/reauth/password')) return { reauth_token: 'proof' }
    if (path.endsWith('/passkeys/key-1') && options?.method === 'DELETE') return await new Promise((_, reject) => { fixture.rejectDelete = reject })
    return {}
  }) } }
})

describe('passkey deletion submission', () => {
  beforeEach(() => { fixture.rejectDelete = undefined })

  it('keeps a delayed delete open through Escape and restores Cancel after failure', async () => {
    const pinia = createPinia()
    const session = useSessionStore(pinia)
    session.session = { authenticated: true, username: 'listener', role: 'user', csrf_token: 'csrf', passkey_available: true }
    session.loaded = true
    render(SecurityView, { global: { plugins: [pinia] } })
    await screen.findByDisplayValue('Travel key')
    await fireEvent.update(screen.getByLabelText('Current password'), 'a secure password')
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Travel key' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Remove passkey' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Removing passkey…')
    await fireEvent.keyDown(screen.getByRole('alertdialog'), { key: 'Escape' })
    expect(screen.getByRole('alertdialog')).toBeVisible()
    fixture.rejectDelete?.(new Error('delete failed'))
    await waitFor(() => expect(screen.getByRole('alertdialog')).toContainElement(screen.getByRole('alert')))
    expect(screen.getByRole('alert')).toHaveTextContent('The passkey was not removed.')
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus()
  })
})
