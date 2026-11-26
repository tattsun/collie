import { t } from './i18n'
// Passkeys: convert the server's WebAuthn options (base64url JSON) to the
// browser API and the browser's credential back to JSON.

function decode(s: string): ArrayBuffer {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((s.length + 3) % 4)
  const bin = atob(b64)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out.buffer
}

function encode(buf: ArrayBuffer | null | undefined): string {
  if (!buf) return ''
  const bytes = new Uint8Array(buf)
  let bin = ''
  for (const b of bytes) bin += String.fromCharCode(b)
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

export const passkeysSupported = () =>
  typeof window.PublicKeyCredential !== 'undefined' && !!navigator.credentials && window.isSecureContext

export async function createPasskey(options: any): Promise<any> {
  const pk = { ...options.publicKey }
  pk.challenge = decode(pk.challenge)
  pk.user = { ...pk.user, id: decode(pk.user.id) }
  pk.excludeCredentials = (pk.excludeCredentials || []).map((c: any) => ({ ...c, id: decode(c.id) }))
  const cred = (await navigator.credentials.create({ publicKey: pk })) as PublicKeyCredential | null
  if (!cred) throw new Error(t('Passkey creation was cancelled'))
  const r = cred.response as AuthenticatorAttestationResponse
  return {
    id: cred.id,
    rawId: encode(cred.rawId),
    type: cred.type,
    authenticatorAttachment: (cred as any).authenticatorAttachment ?? undefined,
    response: {
      clientDataJSON: encode(r.clientDataJSON),
      attestationObject: encode(r.attestationObject),
      transports: r.getTransports ? r.getTransports() : [],
    },
    clientExtensionResults: cred.getClientExtensionResults(),
  }
}

export async function getPasskey(options: any): Promise<any> {
  const pk = { ...options.publicKey }
  pk.challenge = decode(pk.challenge)
  pk.allowCredentials = (pk.allowCredentials || []).map((c: any) => ({ ...c, id: decode(c.id) }))
  const cred = (await navigator.credentials.get({ publicKey: pk, mediation: options.mediation })) as PublicKeyCredential | null
  if (!cred) throw new Error(t('Sign-in was cancelled'))
  const r = cred.response as AuthenticatorAssertionResponse
  return {
    id: cred.id,
    rawId: encode(cred.rawId),
    type: cred.type,
    authenticatorAttachment: (cred as any).authenticatorAttachment ?? undefined,
    response: {
      clientDataJSON: encode(r.clientDataJSON),
      authenticatorData: encode(r.authenticatorData),
      signature: encode(r.signature),
      userHandle: encode(r.userHandle),
    },
    clientExtensionResults: cred.getClientExtensionResults(),
  }
}

/** Human message for WebAuthn DOMExceptions. */
export function passkeyError(e: unknown): string {
  if (e instanceof DOMException) {
    if (e.name === 'NotAllowedError') return t('Cancelled or timed out.')
    if (e.name === 'InvalidStateError') return t('This device already has a passkey for this account.')
    if (e.name === 'SecurityError') return t('Passkeys need the HTTPS address of Collie.')
  }
  return e instanceof Error ? e.message : String(e)
}
