// Passkeys in the browser. The server sends WebAuthn options as JSON with
// binary fields in base64url; the browser API wants ArrayBuffers and returns
// them. These helpers convert both ways, so they work in every browser that
// has passkeys, not only those with PublicKeyCredential.parse*FromJSON.

type Json = Record<string, any>;
export type PasskeyOptions = { publicKey: Json };

function fromB64url(s: string): ArrayBuffer {
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(s.length / 4) * 4, "=");
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}

function toB64url(buf: ArrayBuffer): string {
  let bin = "";
  for (const b of new Uint8Array(buf)) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

const withIds = (list: Json[] | undefined) => (list ?? []).map((c) => ({ ...c, id: fromB64url(c.id) }));

export const passkeysSupported = () => typeof window !== "undefined" && "PublicKeyCredential" in window;

// The console requests no WebAuthn extensions, so it sends none back: some
// password managers add outputs nobody asked for, which the server refuses.
const noExtensions = {};

/** Creates a passkey and returns the response in the JSON form the server reads. */
export async function createPasskey(options: PasskeyOptions): Promise<Json> {
  const pk = options.publicKey;
  const publicKey = {
    ...pk,
    challenge: fromB64url(pk.challenge),
    user: { ...pk.user, id: fromB64url(pk.user.id) },
    excludeCredentials: withIds(pk.excludeCredentials),
  } as PublicKeyCredentialCreationOptions;
  const cred = (await navigator.credentials.create({ publicKey })) as PublicKeyCredential | null;
  if (!cred) throw new Error("No passkey was created.");
  const res = cred.response as AuthenticatorAttestationResponse;
  return {
    id: cred.id,
    rawId: toB64url(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    clientExtensionResults: noExtensions,
    response: {
      clientDataJSON: toB64url(res.clientDataJSON),
      attestationObject: toB64url(res.attestationObject),
      transports: res.getTransports?.() ?? [],
    },
  };
}

/** Asks for a passkey and returns the assertion in the JSON form the server reads. */
export async function getPasskey(options: PasskeyOptions): Promise<Json> {
  const pk = options.publicKey;
  const publicKey = {
    ...pk,
    challenge: fromB64url(pk.challenge),
    allowCredentials: withIds(pk.allowCredentials),
  } as PublicKeyCredentialRequestOptions;
  const cred = (await navigator.credentials.get({ publicKey })) as PublicKeyCredential | null;
  if (!cred) throw new Error("No passkey was chosen.");
  const res = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: toB64url(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    clientExtensionResults: noExtensions,
    response: {
      clientDataJSON: toB64url(res.clientDataJSON),
      authenticatorData: toB64url(res.authenticatorData),
      signature: toB64url(res.signature),
      userHandle: res.userHandle ? toB64url(res.userHandle) : undefined,
    },
  };
}

/** A sentence for errors thrown by the browser's passkey prompt. */
export function passkeyErrorMessage(e: unknown): string {
  if (e instanceof DOMException) {
    if (e.name === "NotAllowedError") return "The passkey prompt was closed or timed out. Try again.";
    if (e.name === "InvalidStateError") return "This authenticator already has a passkey for your account.";
    if (e.name === "SecurityError") return "Passkeys only work on the console's own address over HTTPS.";
  }
  return e instanceof Error ? e.message : "The passkey could not be used.";
}
