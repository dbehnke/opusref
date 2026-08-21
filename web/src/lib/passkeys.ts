function decode(value: string): ArrayBuffer {
  const normalized = value.replace(/-/g, '+').replace(/_/g, '/')
  const raw = atob(normalized.padEnd(Math.ceil(normalized.length / 4) * 4, '='))
  return Uint8Array.from(raw, character => character.charCodeAt(0)).buffer
}

function encode(value: ArrayBuffer): string {
  const raw = String.fromCharCode(...new Uint8Array(value))
  return btoa(raw).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

export interface CeremonyOptions { ceremony_id: string; publicKey: Record<string, any> }

export function assertionOptions(input: CeremonyOptions): CredentialRequestOptions {
  const publicKey: any = { ...input.publicKey, challenge: decode(input.publicKey.challenge) }
  if (Array.isArray(publicKey.allowCredentials)) publicKey.allowCredentials = publicKey.allowCredentials.map((item: any) => ({ ...item, id: decode(item.id) }))
  return { publicKey: publicKey as PublicKeyCredentialRequestOptions }
}

export function registrationOptions(input: CeremonyOptions): CredentialCreationOptions {
  const publicKey: any = { ...input.publicKey, challenge: decode(input.publicKey.challenge), user: { ...input.publicKey.user, id: decode(input.publicKey.user.id) } }
  if (Array.isArray(publicKey.excludeCredentials)) publicKey.excludeCredentials = publicKey.excludeCredentials.map((item: any) => ({ ...item, id: decode(item.id) }))
  return { publicKey: publicKey as PublicKeyCredentialCreationOptions }
}

export function serializeCredential(credential: PublicKeyCredential) {
  const response = credential.response
  const common = { id: credential.id, rawId: encode(credential.rawId), type: credential.type, authenticatorAttachment: credential.authenticatorAttachment, clientExtensionResults: credential.getClientExtensionResults() }
  if (response instanceof AuthenticatorAssertionResponse) return { ...common, response: { clientDataJSON: encode(response.clientDataJSON), authenticatorData: encode(response.authenticatorData), signature: encode(response.signature), userHandle: response.userHandle ? encode(response.userHandle) : null } }
  const creation = response as AuthenticatorAttestationResponse
  return { ...common, response: { clientDataJSON: encode(creation.clientDataJSON), attestationObject: encode(creation.attestationObject), transports: creation.getTransports?.() ?? [] } }
}
