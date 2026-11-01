// c4se is the Secure Enclave behind a C ABI, for the keystore's cgo build
// (PEOPLE-KEYS-2 3.4, 10.4 item 15): CryptoKit, public API only, built as a
// static library (make keystore-se) and linked into c4-farm with -tags
// c4se. A key never leaves the chip: what the keystore keeps is its sealed
// blob, which only this Mac's Secure Enclave can use, and which any
// program running as the person can use too, so the blob is written 0600.
import CryptoKit
import Foundation
import Security

// c4se_available says whether this Mac has a Secure Enclave.
@_cdecl("c4se_available")
public func c4se_available() -> Int32 {
    return SecureEnclave.isAvailable ? 1 : 0
}

// c4se_make makes a key and copies its blob into blob (cap bytes) and its
// X9.63 public key into pub (65 bytes). kind 0 is a person's device key:
// silent, usable while the Mac is unlocked. kind 1 is their touch key: each
// signature needs the person's presence (Touch ID, or their password where
// there is no sensor). kind 2 is a workload's key (an agent's, a cron
// job's): silent, usable after the Mac's first unlock, behind a locked
// screen too. It returns the blob's length, or -1 with why in errbuf.
@_cdecl("c4se_make")
public func c4se_make(_ kind: Int32, _ blob: UnsafeMutablePointer<UInt8>, _ cap: Int32, _ pub: UnsafeMutablePointer<UInt8>,
                      _ errbuf: UnsafeMutablePointer<CChar>, _ errcap: Int32) -> Int32 {
    var flags: SecAccessControlCreateFlags = [.privateKeyUsage]
    if kind == 1 {
        flags.insert(.userPresence)
    }
    let protection = kind == 2 ? kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly : kSecAttrAccessibleWhenUnlockedThisDeviceOnly
    var cfErr: Unmanaged<CFError>?
    guard let ac = SecAccessControlCreateWithFlags(nil, protection, flags, &cfErr) else {
        return fail(errbuf, errcap, "access control: \(String(describing: cfErr?.takeRetainedValue()))")
    }
    do {
        let key = try SecureEnclave.P256.Signing.PrivateKey(accessControl: ac)
        let d = key.dataRepresentation, p = key.publicKey.x963Representation
        guard d.count <= cap, p.count == 65 else {
            return fail(errbuf, errcap, "the key's blob is \(d.count) bytes")
        }
        d.copyBytes(to: blob, count: d.count)
        p.copyBytes(to: pub, count: 65)
        return Int32(d.count)
    } catch {
        return fail(errbuf, errcap, "\(error)")
    }
}

// c4se_public copies the X9.63 public key of the key in blob into pub (65
// bytes). It returns 65, or -1 with why in errbuf.
@_cdecl("c4se_public")
public func c4se_public(_ blob: UnsafePointer<UInt8>, _ blen: Int32, _ pub: UnsafeMutablePointer<UInt8>,
                        _ errbuf: UnsafeMutablePointer<CChar>, _ errcap: Int32) -> Int32 {
    do {
        let key = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: Data(bytes: blob, count: Int(blen)))
        let p = key.publicKey.x963Representation
        p.copyBytes(to: pub, count: 65)
        return 65
    } catch {
        return fail(errbuf, errcap, "\(error)")
    }
}

// c4se_sign signs msg (SHA-256, ECDSA) with the key in blob, writing the
// signature raw, r||s, into sig (64 bytes); the caller folds S low. A touch
// key asks for the person's presence here. It returns 64, or -1 with why
// in errbuf.
@_cdecl("c4se_sign")
public func c4se_sign(_ blob: UnsafePointer<UInt8>, _ blen: Int32, _ msg: UnsafePointer<UInt8>, _ mlen: Int32,
                      _ sig: UnsafeMutablePointer<UInt8>, _ errbuf: UnsafeMutablePointer<CChar>, _ errcap: Int32) -> Int32 {
    do {
        let key = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: Data(bytes: blob, count: Int(blen)))
        let raw = try key.signature(for: Data(bytes: msg, count: Int(mlen))).rawRepresentation
        guard raw.count == 64 else {
            return fail(errbuf, errcap, "the signature is \(raw.count) bytes")
        }
        raw.copyBytes(to: sig, count: 64)
        return 64
    } catch {
        return fail(errbuf, errcap, "\(error)")
    }
}

private func fail(_ errbuf: UnsafeMutablePointer<CChar>, _ errcap: Int32, _ why: String) -> Int32 {
    let n = min(Int(errcap) - 1, why.utf8.count)
    if n > 0 {
        _ = why.withCString { strncpy(errbuf, $0, n) }
        errbuf[n] = 0
    }
    return -1
}
