import { describe, expect, it, vi } from 'vitest'
import { assertionOptions, registrationOptions, serializeCredential } from '../src/lib/passkeys'

const encoded = (bytes: number[]) => btoa(String.fromCharCode(...bytes)).replace(/=/g, '').replace(/\+/g, '-').replace(/\//g, '_')

describe('WebAuthn JSON conversion', () => {
  it('decodes flattened assertion options from the backend contract', () => {
    const result = assertionOptions({ ceremony_id: 'login-1', publicKey: { challenge: encoded([1, 2, 3]), rpId: 'radio.example', allowCredentials: [{ type: 'public-key', id: encoded([4, 5]) }] } })
    expect([...new Uint8Array(result.publicKey!.challenge as ArrayBuffer)]).toEqual([1, 2, 3])
    expect([...new Uint8Array(result.publicKey!.allowCredentials![0]!.id as ArrayBuffer)]).toEqual([4, 5])
  })

  it('decodes flattened registration user and credential IDs', () => {
    const result = registrationOptions({ ceremony_id: 'enroll-1', publicKey: { challenge: encoded([1]), rp: { id: 'radio.example', name: 'Radio' }, user: { id: encoded([2]), name: 'user', displayName: 'User' }, pubKeyCredParams: [{ type: 'public-key', alg: -7 }], excludeCredentials: [{ type: 'public-key', id: encoded([3]) }] } })
    expect([...new Uint8Array(result.publicKey!.user.id as ArrayBuffer)]).toEqual([2])
    expect([...new Uint8Array(result.publicKey!.excludeCredentials![0]!.id as ArrayBuffer)]).toEqual([3])
  })

  it('serializes assertion bytes without losing URL-safe base64 data', () => {
    class AssertionResponse {}
    vi.stubGlobal('AuthenticatorAssertionResponse', AssertionResponse)
    const response = new AssertionResponse()
    Object.assign(response, { clientDataJSON: new Uint8Array([1]).buffer, authenticatorData: new Uint8Array([2]).buffer, signature: new Uint8Array([3]).buffer, userHandle: new Uint8Array([4]).buffer })
    const credential = { id: 'credential', rawId: new Uint8Array([5]).buffer, type: 'public-key', authenticatorAttachment: 'platform', getClientExtensionResults: vi.fn(() => ({})), response } as unknown as PublicKeyCredential
    expect(serializeCredential(credential)).toMatchObject({ rawId: encoded([5]), response: { clientDataJSON: encoded([1]), authenticatorData: encoded([2]), signature: encoded([3]), userHandle: encoded([4]) } })
  })
})
