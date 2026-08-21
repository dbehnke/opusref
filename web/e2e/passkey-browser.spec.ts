import { expect, test } from '@playwright/test'

test('browser creates and asserts a discoverable passkey with flattened options', async ({ page, browserName }) => {
  test.skip(browserName !== 'chromium', 'The virtual authenticator uses the Chromium DevTools protocol.')
  const cdp = await page.context().newCDPSession(page)
  await cdp.send('WebAuthn.enable')
  const authenticator = await cdp.send('WebAuthn.addVirtualAuthenticator', { options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true } })
  try {
    await page.goto('http://localhost:4173/login')
    const result = await page.evaluate(async () => {
      const { registrationOptions, assertionOptions, serializeCredential } = await import('/src/lib/passkeys.ts')
      const base64url = (value: Uint8Array) => btoa(String.fromCharCode(...value)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
      const registration = registrationOptions({ ceremony_id: 'register', publicKey: { challenge: base64url(crypto.getRandomValues(new Uint8Array(32))), rp: { id: 'localhost', name: 'OpusRef' }, user: { id: base64url(crypto.getRandomValues(new Uint8Array(32))), name: 'operator', displayName: 'Operator' }, pubKeyCredParams: [{ type: 'public-key', alg: -7 }], timeout: 60000, authenticatorSelection: { residentKey: 'required', requireResidentKey: true, userVerification: 'required' }, attestation: 'none' } })
      const created = await navigator.credentials.create(registration) as PublicKeyCredential
      const creationJSON = serializeCredential(created)
      const assertion = assertionOptions({ ceremony_id: 'assert', publicKey: { challenge: base64url(crypto.getRandomValues(new Uint8Array(32))), rpId: 'localhost', timeout: 60000, userVerification: 'required' } })
      const asserted = await navigator.credentials.get(assertion) as PublicKeyCredential
      const assertionJSON = serializeCredential(asserted)
      return { createdID: created.id, assertedID: asserted.id, creationJSON, assertionJSON }
    })
    expect(result.createdID).toBe(result.assertedID)
    expect(result.creationJSON.response.attestationObject).toBeTruthy()
    expect(result.assertionJSON.response.signature).toBeTruthy()
    expect(result.assertionJSON.response.userHandle).toBeTruthy()
  } finally {
    await cdp.send('WebAuthn.removeVirtualAuthenticator', { authenticatorId: authenticator.authenticatorId })
  }
})
