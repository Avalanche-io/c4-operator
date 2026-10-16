# Provenance standards brief for c4-operator

Prepared 2026-10-16. Each claim is followed by its source. "Unverified" marks anything that could not be confirmed against a primary or reputable secondary source in this session. "Required" and "recommended" follow the source's own language (MUST/SHOULD, "shall", contract text). Where only a secondary source was reachable, the source is a law firm, trade press, or a summary page, and that is stated.

---

## 1. in-toto Attestation Framework

### Status and version

- The current framework release is v1.2.0, published 2026-03-18. Earlier releases: v1.1.2 (2025-06-14), v1.1.1 (2025-01-24), v1.1.0 (2024-05-28), v1.0.2 (2024-04-08), v1.0.1 (2023-12-20), v1.0 (2023-03-22). Source: https://github.com/in-toto/attestation/releases (dates from the GitHub releases API).
- v1.2.0 added the Simple Verification Result (SVR) predicate, Release predicate v0.2, an SPDX 3 predicate, Rust bindings and envelope spec updates. Source: https://github.com/in-toto/attestation/releases
- v1.1.0 generalized `DigestSet` to allow any immutable identifier, not only cryptographic digests. Source: https://github.com/in-toto/attestation/releases
- The framework has four layers: Predicate, Statement, Envelope, Bundle. Releases track SemVer MAJOR.MINOR of the spec; a major version change would move the Statement `_type` to v2. Source: https://github.com/in-toto/attestation/blob/main/spec/README.md
- The Statement wire type is still `https://in-toto.io/Statement/v1` in v1.2. Source: https://github.com/in-toto/attestation/blob/main/spec/v1/statement.md

### Statement v1 fields

- `_type` (required): always `https://in-toto.io/Statement/v1`. Source: https://github.com/in-toto/attestation/blob/main/spec/v1/statement.md
- `subject` (required): array of ResourceDescriptor; each subject MUST have `digest` set. Subjects are assumed immutable. `name` is optional ("_" if meaningless); `name` and `uri` SHOULD be unique within `subject`. Subjects are "matched purely by digest, regardless of content type." Source: https://github.com/in-toto/attestation/blob/main/spec/v1/statement.md
- `predicateType` (required): a TypeURI. `predicate` (optional) is an arbitrary JSON object; unset is the same as empty. Source: https://github.com/in-toto/attestation/blob/main/spec/v1/predicate.md
- ResourceDescriptor fields: `name`, `uri`, `digest`, `content` (inline bytes, keep under 1 KB), `downloadLocation`, `mediaType` (use `x.`, `prs.` or `vnd.` prefixes for non-standard types), `annotations` (map of any JSON). At least one of `uri`, `digest`, `content` MUST be set. Source: https://github.com/in-toto/attestation/blob/main/spec/v1/resource_descriptor.md

### Digest maps (DigestSet) and non-standard algorithm names

- A DigestSet is a JSON object mapping algorithm name to an encoded value. Named standard algorithms use lowercase hex. Multiple entries MAY be used for algorithm agility. Source: https://github.com/in-toto/attestation/blob/main/spec/v1/digest_set.md
- `sha512` is a predefined name (NIST names, lowercased, `-` replaced by `_`), alongside `sha256`, `sha384`, `sha3_*`, `blake2b`, and others; also `dirHash` and `gitCommit`/`gitTree`/`gitBlob`/`gitTag`. Source: https://github.com/in-toto/attestation/blob/main/spec/v1/digest_set.md
- Custom names are allowed. The spec's own example `{"somecoolhash": "abcd"}` "uses a non-predefined algorithm", and the rule is "New algorithms MUST document how the value is encoded, e.g. URL-safe base64, lowercase hex, etc." Source: https://github.com/in-toto/attestation/blob/main/spec/v1/digest_set.md
- Consumers "MUST only accept algorithms that they consider secure and MUST ignore unrecognized or unaccepted algorithms." Two DigestSets SHOULD match if ANY acceptable field matches. It is RECOMMENDED to use at least `sha256` for compatibility. Source: https://github.com/in-toto/attestation/blob/main/spec/v1/digest_set.md
- Consequence: a `"c4"` key is spec-legal if its encoding is documented, but a generic verifier will ignore it. A `"sha512"` key holding the lowercase-hex SHA-512 is understood by generic tooling. (Inference from the rules above.)

### Envelope (DSSE)

- The RECOMMENDED envelope is DSSE v1.0. `payloadType` MUST be `application/vnd.in-toto+json` or `application/vnd.in-toto.<predicate>+json`, and MUST be signed with the payload. `payload` MUST be a base64-encoded JSON Statement. `signatures` is REQUIRED; a `keyid` SHOULD be included per key. Source: https://github.com/in-toto/attestation/blob/main/spec/v1/envelope.md
- DSSE JSON envelope: `{"payload", "payloadType", "signatures": [{"keyid", "sig"}]}`; `payload`, `payloadType`, `signatures`, `sig` required; `keyid` optional. Multiple signatures are allowed. Envelope doc is version 1.0.2, dated May 10, 2024. Source: https://github.com/secure-systems-lab/dsse/blob/master/envelope.md
- The signature is computed over PAE: `"DSSEv1" SP LEN(type) SP type SP LEN(body) SP body`. KEYID is an unauthenticated hint that MUST NOT be used for security decisions. Source: https://github.com/secure-systems-lab/dsse/blob/master/protocol.md

### Predicate type conventions

- `predicateType` is a TypeURI: case-normalized URI that "SHOULD resolve to a human-readable description", "SHOULD include a version number", and is not registered ("The natural namespacing of URIs is sufficient"). Source: https://github.com/in-toto/attestation/blob/main/spec/v1/field_types.md
- New predicate types MAY be vetted by the maintainers; vetting is optional. Source: https://github.com/in-toto/attestation/blob/main/spec/v1/predicate.md
- Vetted predicates include SLSA Provenance, SLSA VSA, SCAI, Link, Reference, Release, Runtime Traces, SPDX 2/3, CycloneDX, SVR, Test Result, VULNS. No vetted predicate for datasets, media, or ML training was found. Source: https://github.com/in-toto/attestation/tree/main/spec/predicates

---

## 2. SLSA

### Version

- The current approved SLSA specification is v1.2, announced 24 November 2025. Its main addition is the Source Track. It is backward compatible with v1.1. The Build Environment and Dependency tracks are still in development. Sources: https://slsa.dev/spec/ and https://slsa.dev/blog/2025/11/announce-slsa-v1.2
- Approved tracks: Build and Source. Source: https://slsa.dev/spec/

### Provenance predicate (Build track)

- `predicateType` is `https://slsa.dev/provenance/v1` (resolves to the latest minor). Source: https://slsa.dev/spec/v1.2/build-provenance
- Required: `buildDefinition.buildType`, `buildDefinition.externalParameters`, `runDetails.builder.id`. Optional: `internalParameters`, `resolvedDependencies`, `builder.version`, `builder.builderDependencies`, `metadata.invocationId`, `metadata.startedOn`, `metadata.finishedOn`, `byproducts`. Source: https://slsa.dev/spec/v1.2/build-provenance
- `externalParameters` are inputs under tenant control and "MUST be complete at SLSA Build L3"; verifiers should reject unrecognized fields. `internalParameters` are platform-set, for debugging, and need not be verified. `resolvedDependencies` are artifacts fetched during the build; completeness is best effort through L3. `builder.id` is a URI for the trusted build platform and determines the Build level. Consumers MUST accept only specific signer-builder pairs. Source: https://slsa.dev/spec/v1.2/build-provenance

### What build levels require of the provenance generator

- L1: provenance exists and "unambiguously identifies the output package by cryptographic digest"; no authenticity requirement. Source: https://slsa.dev/spec/v1.2/build-requirements
- L2: hosted build platform; provenance is "generated by the control plane" (not the tenant) and signed with "a private key accessible only to the build platform component that generated the provenance attestation." Source: https://slsa.dev/spec/v1.2/build-requirements
- L3: provenance "strongly resistant to forgery by tenants" (signing secrets unreachable from user build steps); builds isolated so "two builds that overlap in time" cannot influence each other; ephemeral environments; cache poisoning prevented. L3 only ensures "a well-intentioned build runs securely." Source: https://slsa.dev/spec/v1.2/build-requirements
- Producers MUST select a platform capable of the desired level and MUST distribute provenance to consumers. Source: https://slsa.dev/spec/v1.2/build-requirements

### SLSA beyond software builds

- The SLSA v1.2 announcement and spec do not mention data, media or ML models. No SLSA data or media track exists. Sources: https://slsa.dev/blog/2025/11/announce-slsa-v1.2 and https://slsa.dev/spec/
- Adjacent work: OpenSSF Model Signing (OMS) spec v1.0.0 (2025-04-04), v1.1.0/v1.1.1 (2025-10-10). OMS signatures are detached Sigstore bundles containing an in-toto Statement whose manifest lists every file by hash (SHA-256 default, BLAKE2b alternative), PKI-agnostic (bare keys, X.509 chains, Sigstore keyless). Source: https://github.com/ossf/model-signing-spec (release dates from GitHub API). The `model-signing` 1.0 library was announced by Sigstore. Source: https://blog.sigstore.dev/model-transparency-v1.0
- An OpenSSF AI/ML WG proposal for a model-lifecycle provenance metadata format is open; it is a proposal, not a spec. Source: https://github.com/ossf/ai-ml-security/issues/40
- Atlas (Intel Labs, Feb 2025 paper) proposes applying SLSA build provenance across ML lifecycle stages. Research, not a standard. Source: https://arxiv.org/html/2502.19567v1

---

## 3. C2PA (Content Credentials)

### Version

- Current technical specification: 2.4, "April 2026". Version history in the spec: 2.3 December 2025, 2.2 May 2025, 2.1 September 2024, 2.0 January 2024. Source: https://spec.c2pa.org/specifications/specifications/2.4/specs/C2PA_Specification.html (section 5.3)
- 2.4 adds: crJSON (a JSON-LD derived view, "not independently verifiable"), `c2pa.repository-receipt`, `c2pa.environmental-sustainability`, `c2pa.ai-disclosure`, HTML and structured-text embedding; moves `specVersion` into `claim_generator_info`. Source: same spec, section 5.3.1
- 2.3 added live video (per-segment signing), plain text, OGG Vorbis, large AVI, and declared trust list version for ingredient validation. Sources: https://c2pa.org/the-c2pa-launches-content-credentials-2-3-and-celebrates-5-years-of-impact-across-the-digital-ecosystem/ and the 2.4 spec section 5.3.2

### What a manifest contains

- A C2PA Manifest holds an assertion store, a claim, and a claim signature; a Manifest Store holds the active manifest (last) plus ingredient manifests, serialized as JUMBF (ISO 19566-5:2023). Source: 2.4 spec, sections 11.1 to 11.3 (URL above)
- Claims split assertions into `created_assertions` and `gathered_assertions`. The mandatory actions assertion must be in `created_assertions` (2.4). Source: 2.4 spec, section 5.3.1 and 18.15.2
- Exactly one hard binding is required in a standard manifest: `c2pa.hash.data`, `c2pa.hash.boxes`, `c2pa.hash.collection.data`, `c2pa.hash.bmff.v2` (deprecated), or `c2pa.hash.bmff.v3`. Source: 2.4 spec, section 11.1 (claim types) and 15.x validation
- Allowed hash algorithms include SHA2-512 (`"sha512"`); SHA-3 is not allowed. Source: 2.4 spec, section 13.1
- Ingredients use `c2pa.ingredient.v3`, with relationships such as `parentOf` (at most one), `componentOf` and `inputTo`. 2.4 allows `digitalSourceType` on ingredients that lack their own manifest. Source: 2.4 spec
- Signer trust: validators must include the C2PA Trust List for the `c2pa-kp-claimSigning` EKU. Source: 2.4 spec, section 14 (line referencing OID 1.3.6.1.4.1.62558.2.1)

### Actions and digitalSourceType

- Every asset's first `c2pa.actions` assertion must start with `c2pa.created` (de novo) or `c2pa.opened`; `c2pa.created` "shall" carry a `digitalSourceType`. Source: 2.4 spec, section 18.15.2 and 18.15 action list
- Predefined actions include `c2pa.created`, `c2pa.opened`, `c2pa.edited`, `c2pa.edited.metadata`, `c2pa.placed`, `c2pa.color_adjustments`, `c2pa.cropped`, `c2pa.resized`, `c2pa.transcoded`, `c2pa.repackaged`, `c2pa.converted`, `c2pa.trimmed`, `c2pa.deleted`, `c2pa.removed`, `c2pa.translated`, `c2pa.redacted`, `c2pa.published`, `c2pa.watermarked.bound`, `c2pa.watermarked.unbound`, `c2pa.unknown`. Source: 2.4 spec, section 18.15
- `digitalSourceType` values come from the IPTC vocabulary (for example `http://cv.iptc.org/newscodes/digitalsourcetype/trainedAlgorithmicMedia`, `compositeSynthetic`, `humanEdits`, `digitalCapture`) plus C2PA values `http://c2pa.org/digitalsourcetype/empty` and `http://c2pa.org/digitalsourcetype/trainedAlgorithmicData`. Source: 2.4 spec, section 18.15. `compositeWithTrainedAlgorithmicMedia` appears in the 2.4 AI disclosure table. Source: 2.4 spec, section 18.28.3
- New in 2.4: `c2pa.ai-disclosure` with required `modelType` and optional `modelName`, `modelIdentifier`, `contentProfile.humanOversightLevel` (`fully_autonomous`, `prompt_guided`, `human_validated`), `scientificDomain` (arXiv taxonomy). Fields like `trainingCleared` are listed as pending. Source: 2.4 spec, section 18.28

### Training and data mining assertion

- The C2PA "Training & Data Mining" assertion was removed from the C2PA spec in 2.0. Source: 2.4 spec, section 5.3.5 change list
- It now lives in the Creator Assertions Working Group (DIF) spec "Training and Data Mining Assertion" v1.1, DIF Ratified 16 May 2025, label `cawg.training-mining`, entries `cawg.data_mining`, `cawg.ai_inference`, `cawg.ai_generative_training`, `cawg.ai_training`, values `allowed` / `notAllowed` / `constrained` (constrained treated as notAllowed absent more info), optional `constraint_info`. Source: https://cawg.io/training-and-data-mining/1.1/

### File formats

- Embedding is defined for: JPEG, JPEG XL, PNG, SVG, HTML, FLAC, MP3, GIF, DNG and TIFF-based, WAV/BWF, AVI, WebP and other RIFF, fonts, unstructured and structured text, PDF, EPUB, OOXML, ODF, OpenXPS, other ZIP formats, MP4, MOV, AAC, ALAC, HEIF, OGG Vorbis, and "Other BMFF-based formats". Source: 2.4 spec, Appendix A.1
- OpenEXR and MXF are not in the 2.4 supported-format list. Source: 2.4 spec, Appendix A.1
- ProRes in a QuickTime `.mov` is covered through the MOV/BMFF path (`c2pa.hash.bmff.v3`, with Merkle-tree hashing for large or fragmented files). This is an inference: the spec names MOV, and ProRes is a codec inside MOV; ProRes in MXF has no defined embedding. Source: 2.4 spec, Appendix A.1 and A.5
- External (sidecar) manifests: a Manifest Store may be stored outside the asset, served as `application/c2pa`. Validators "should look for files at the same path or URI, but with a filename extension of `.c2pa`", or an HTTP `Link` header with `rel=c2pa-manifest`; XMP `dcterms:provenance` can point to a remote manifest. Source: 2.4 spec, sections 11.4, 11.5 and 15.x manifest discovery
- Collections: `c2pa.hash.collection.data` hard-binds a list of URIs with per-file hashes, intended where "the C2PA Manifest will refer to a collection of assets"; the spec's example is folders of an AI training set. This is the defined mechanism closest to an EXR frame sequence. Source: 2.4 spec, section 18.8
- Multi-part assets: a primary part carries a manifest with `c2pa.hash.multi-asset` describing each part. Source: 2.4 spec, Appendix A.2
- OpenEXR: GitHub issue #1497 (opened 2023-07-26 by the C2PA TWG chair) was closed 2026-04-25. OpenEXR v3.4 (2025-09-05) added a generic `bytes` attribute with a type hint; a C2PA user could use hint `org.c2pa.jumbf`, but the maintainer noted this is not standardized and suggested a new issue for first-class support. Sources: https://github.com/AcademySoftwareFoundation/openexr/issues/1497 and https://github.com/AcademySoftwareFoundation/openexr/releases/tag/v3.4.0
- MXF: SMPTE formed a Content Provenance and Authenticity in Media Study Group in July 2025, with MXF carriage as a key focus. No published report or standard was found as of this brief. Source: https://www.smpte.org/blog/smpte-unveils-content-provenance-and-authenticity-in-media-study-group (status of any 2026 report: unverified)

### Trust list and conformance

- The C2PA Conformance Program launched 4 June 2025. As of 14 May 2026, 54 products from 31 companies had passed, and over 120 more companies had submitted expressions of interest. Source: https://c2pa.org/wp-content/uploads/sites/33/2026/07/Content-Credentials-Deployment-Guidance.pdf (v1.0, 2026-07-08)
- Generator products must sign with an X.509 certificate from a CA on the C2PA Trust List. There are two assurance levels, encoded as a certificate extension. Process: expression of interest, legal agreement, evidence, and for generators a product security architecture template. Source: https://opensource.contentauthenticity.org/docs/conformance/
- The Interim Trust List was frozen 1 January 2026; validators should move to the official C2PA Trust List and must distinguish ITL-signed credentials. There is also a C2PA TSA Trust List for time-stamp authorities. Source: https://opensource.contentauthenticity.org/docs/conformance/trust-lists
- Validators show "Valid" (intact, signer possibly not on the list) versus "Trusted" (signer on the C2PA Trust List). Source: https://c2pa.org/wp-content/uploads/sites/33/2026/07/Content-Credentials-Deployment-Guidance.pdf

### Attaching credentials to deliverables (synthesis)

- ProRes `.mov` and H.264/HEVC `.mp4`: embed via BMFF hashing (spec-defined).
- MXF (including ProRes or JPEG 2000 in MXF, IMF track files): no embedding defined; use a sidecar `.c2pa` at the same path, or a manifest repository plus HTTP `Link` header. Validators will look for the sidecar, but MXF-aware tools are not required to.
- EXR sequences: no embedding defined; one practical option is a single manifest with `c2pa.hash.collection.data` over the frame files, stored as a sidecar `.c2pa` for the sequence directory. Per-frame JUMBF in the EXR `bytes` attribute is possible but non-standard.
- WAV/BWF audio stems and PDF/DOCX paperwork: embed (spec-defined).
- A deliverable only shows "Trusted" in conforming validators if the signing certificate chains to the C2PA Trust List, which requires going through the conformance program.

(All of the above derives from the 2.4 spec, Appendix A and sections 11.4 and 18.8, and the conformance sources cited above.)

---

## 4. MovieLabs and ASWF

### Ontology for Media Creation (OMC)

- Current release: OMC v2.8, announced 3 January 2026, a "non-breaking update": Collections, UDIM and OpenPBR for CG assets, custom data handling, validator updates. v3.0 was announced for "later in 2026" to cover capture, post, VFX handoffs, mastering and distribution. No v3.0 release was found as of this brief. Source: https://movielabs.com/news/movielabs-releases-ontology-for-media-creation-v2-8/
- The JSON schema in the repo is `OMC-JSON-v2.8.schema.json`; the last commits were February 2026. Source: https://github.com/MovieLabs/OMC/tree/main/OMC-JSON
- Core concepts: Participants, Tasks, Assets, Contexts. Source: https://movielabs.com/ontology-for-media-creation/ . The v2.8 JSON schema's top-level entity types are `Asset`, `Infrastructure`, `MediaCreationContext`, `Participant`, `Task`, `Utility`. Source: https://github.com/MovieLabs/OMC/blob/main/OMC-JSON/OMC-JSON-v2.8.schema.json
- Lineage-relevant terms in the v2.8 schema:
  - `provenance` ("A record of when something was changed and by whom") with `CreatedBy` (Participant), `createdOn`, `Role`, `Origin` (Asset), `reason`, `annotation`. It is attached to Asset, AssetSC (structural characteristics), Composition and Sequence.
  - Asset `version` with `DerivationOf`, `RevisionOf`, `VariantOf`, `Alternative`, and inverse lists `Derivation`, `Revision`, `Variant`; plus `versionNumber`.
  - `identifier` array with `identifierScope`, `identifierValue`, `combinedForm` (example `URN:ID:IdentifierScope:IdentifierValue`) and `url`.
  - `software` with `softwareName`, `softwareVersion`, `apiVersion`.

  Source: https://github.com/MovieLabs/OMC/blob/main/OMC-JSON/OMC-JSON-v2.8.schema.json
- MovieLabs' versions model has five kinds: Revision, Variant, Derivation, Representation, Alternative. Provenance links an Asset, a Source and who/when/why. Source: https://movielabs.com/versions-in-the-movielabs-ontology-for-media-creation/ (24 April 2024)
- No OMC term for content hashes or checksums was found in the v2.8 schema search. C4 IDs would go in `identifier` with a scope. (Search of the schema for hash/checksum keys returned none.)

### 2030 Vision principles (2019)

From "The Evolution of Media Creation" (2019), source: https://MovieLabs.com/prodtech/ML_2030_Vision.pdf

1. All assets are created or ingested straight into the cloud and do not need to be moved.
2. Applications come to the media.
3. Propagation and distribution of assets is a "publish" function.
4. Archives are deep libraries with access policies matching speed, availability and security to the economics of the cloud.
5. Preservation of digital assets includes the future means to access and edit them.
6. Every individual on a project is identified and verified, and their access permissions are efficiently and consistently managed.
7. All media creation happens in a highly secure environment that adapts rapidly to changing threats.
8. Individual media elements are referenced, accessed, tracked and interrelated using a universal linking system.
9. Media workflows are non-destructive and dynamically created using common interfaces, underlying data formats and metadata.
10. Workflows are designed around real-time iteration and feedback.

### Security (CSAP)

- Common Security Architecture for Production: a zero trust architecture for media creation. Parts 1 Architecture, 2 Interfaces, 3 Security Levels, 4 Securing Software-Defined Workflow, 5A Starting Out (Zero Trust Foundation), 5B Core, 5C Approaches. Current package v1.3; "The latest update was published in August 2023." Security levels 100/200/300. Source: https://movielabs.com/production-technology/production-security/
- No CSAP update addressing AI agents was found. Unverified whether one is in draft.

### MovieLabs on AI

- "Building Foundations for Trusted and Reliable AI in Media Creation Workflows" (Rich Berger, 8 July 2026) names four focus areas: Metadata and Context, Transparency and Provenance, Security and Identity, Change Management (via the MovieLabs Industry Forum). It states the aim to "capture and preserve information about asset lineage" and record human and AI contributions, tied to copyrightability and contractual compliance. It does not specify a schema, model/prompt tracking, or OMC extensions. Source: https://movielabs.com/building-foundations-for-trusted-and-reliable-ai-in-media-creation-workflows/
- No published MovieLabs AI provenance specification was found as of this brief.

### ASWF projects

Lifecycle stages below come from the ASWF landscape file. Source: https://github.com/AcademySoftwareFoundation/aswf-landscape/blob/main/landscape.yml

- **OpenAssetIO**: Sandbox. v1.0.0 2025-06-19, v1.0.2 2026-04-21. An interoperability API between tools and asset management systems, aimed at preserving "identity, purpose or heritage" of data rather than file paths. It is a resolver and publish API, not a provenance record. Sources: https://github.com/OpenAssetIO/OpenAssetIO/releases and https://github.com/OpenAssetIO/OpenAssetIO
- **OpenTimelineIO**: Incubating per the landscape file. v0.18.0 2025-11-06, v0.18.1 2025-11-09. Editorial timeline interchange; references media, does not do provenance. Source: https://github.com/AcademySoftwareFoundation/OpenTimelineIO/releases
- **OpenEXR**: Graduated (Adopted). Latest v3.5.2 (2026-10-03). C2PA status as in section 3. Source: https://github.com/AcademySoftwareFoundation/openexr/releases
- **Working groups** in the landscape: Open Review Initiative and a Zero Trust Working Group. No ASWF provenance, content credentials, or AI working group was found. Source: landscape.yml above.

---

## 5. AI provisions, disclosure and regulation

### WGA

- 2023 MBA (summarized by WGA, page updated 2025-12-18):
  - "The Company must disclose to the writer if any materials given to the writer have been generated by AI."
  - AI or GAI output cannot be considered literary material.
  - A writer can choose to use AI if the company consents, but "the company can't require the writer to use AI software."
  - The WGA "reserves the right to assert that exploitation of writers' material to train AI is prohibited."

  Source: https://www.wga.org/contracts/know-your-rights/artificial-intelligence
- 2026 MBA: ratified 24 April 2026 (90.38% yes), term 2 May 2026 to 1 May 2030. Source: https://www.wga.org/news-events/news/press/wga-ratifies-2026-minimum-basic-agreement-with-amptp
- 2026 AI terms: preserves all 2023 protections, and adds that companies "must give the Guild written notice if they license writers' work to train a commercial GAI system" to create outputs. The Guild may request discussion of the license, "including any remuneration for writers". No new disclosure or credit terms. Source: https://www.wga.org/contracts/contracts/mba/summary-of-the-2026-wga-mba

### SAG-AFTRA

- 2023 TV/Theatrical (law firm summary; SAG-AFTRA pages returned HTTP 403 to this session):
  - Employment-based digital replicas need explicit signed/initialed consent, with compensation terms set out, and are limited to the employing project unless further consent is given.
  - Independently created digital replicas need consent and bargaining (unless as performed or scripted), with pension and health contributions; First Amendment-type uses are exempt.
  - Digital alteration needs consent unless as performed or scripted.
  - Synthetic performers require "Notice to Union and an opportunity to bargain in good faith" when replacing a human performer; consent is needed if a principal facial feature is recognizable as a specific performer.

  Source: https://www.loeb.com/en/insights/passle/2023/11/artificial-intelligence-terms-of-sagaftra-tv-theatrical-contract (13 Nov 2023)
- The specific "48 hours notice" and "reasonably specific description" consent wording commonly cited for 2023 could not be verified from the contract text in this session. Unverified.
- 2026 TV/Theatrical: ratified 4 June 2026 (91.42%), term 1 July 2026 to 30 June 2030. Synthetic performers are permitted only where they bring "significant additional value" compared with a live actor or that actor's digital avatar, with an arbitration provision. Source: https://lamag.com/arts-and-entertainment/sag-aftra-approves-amptp-deal-with-expanded-ai-protections-in-landslide-vote/
- Other reported 2026 items (minimum pay and residuals for independently created replicas, an "articulable business reason" for scans, replicas not crossing picket lines, minors' protections, biometric data and secure storage) come from secondary summaries only. Unverified against SAG-AFTRA text. Sources: https://infinitecreation.io/tutorial-ai-digital-replicas-sag-aftra-2026 and https://www.sagaftra.org/contracts-industry-resources/contracts/2026-tvtheatrical-contracts (not fetchable)

### DGA

- 2023: "Groundbreaking agreement confirming that AI is not a person and that generative AI cannot replace the duties performed by members." Source: https://www.dga.org/News/PressReleases/2023/230603-DGA-and-AMPTP-Reach-Tentative-Agreement
- Press reporting says 2023 also requires notice/consultation with directors on GAI use. Unverified against the contract text.
- 2026: tentative 9 June 2026, ratified (announced 25/26 June 2026), four years. AI terms: renewed 2023 safeguards ("our work must continue to be performed by a person"), plus the agreement "Establishes Director's control over AI-generated footage and creates a new employer-funded skills enhancement program." Sources: https://www.dga.org/news/pressreleases/2026/260612_dga_board_recommends_ratification_of_new_contract and https://www.dga.org/news/pressreleases/2026/260625_dga_membership_ratifies_new_contract_by_overwhelming_margin
- Press reports that studios agreed to meet with the DGA if they license material for AI training. Unverified in DGA's own text.

### IATSE Basic Agreement 2024 (primary MOA text, dated July 3, 2024)

Term: 1 August 2024 to 31 July 2027. Source for every item below: https://laborcenter.berkeley.edu/wp-content/uploads/2025/05/IATSE-PCBA46.pdf (Item 13, Article [XLIX] Artificial Intelligence)

- The Producer may require employees to use AI Systems; employees doing so (including by "inputting prompts") remain covered.
- "The Producer will not require an employee to provide prompts ... in a manner that results in the displacement of any covered employee."
- Employees using AI must follow Producer policies (ethics, privacy, security, copyrightability). The Producer may require consent before AI use and may reject AI output, including where it "could adversely affect the copyrightability" of the work. Producers must give the International written AI policies.
- A required AI use is "subject to consultation with the employee at the employee's request", time permitting.
- Producer indemnifies employees for AI-related claims, conditional on (among others) the employee having "made appropriate disclosure of the use of AI Systems to the Producer."
- Semi-annual industry meetings and quarterly company meetings (on Union request) at which Producers identify AI systems in use or intended use.
- Claims are arbitrable. No injunctive relief; the arbitrator cannot restrict AI use.

### The Animation Guild (Local 839), 2024

- Ratified December 2024 (76.1%), three years (2024-27). AI terms: written notifications, and the ability to consult with production about alternative non-GenAI tools. No opt-out from required AI use, and no bar on use of work for training. Sources: https://thewrap.com/animation-guild-ratifies-2024-deal (23 Dec 2024) and https://www.screenglobalproduction.com/news/2024/11/25/hollywood-animation-guild-amptp-reach-tentative-deal (trade press; contract text not reviewed)

### Distributor and streamer policy (verifiable example)

- Netflix, "Using Generative AI in Content Production" (partner guidance, August 2025):
  - Five principles: no replication of unowned material; tools must not store, reuse or train on production data; enterprise-secured tools where possible; generated material is temporary and not part of final deliverables; no replacing talent or union work without consent.
  - Partners should declare intended GenAI use, and on-screen GenAI content should be flagged early.
  - Written approval is needed for: Netflix-owned or personal data inputs; training on third-party talent assets; generating main characters, key visual elements or settings; significant performance alterations.

  Source: https://www.tvbeurope.com/artificial-intelligence/netflix-publishes-genai-guidance-for-creators (secondary; the Netflix partner page at https://partnerhelp.netflixstudios.com/hc/en-us/articles/43393929218323-Using-Generative-AI-in-Content-Production now redirects to a JavaScript portal that could not be read)
- Other studio or streamer AI policies were not verified in this session.

### EU AI Act Article 50

- Art. 50(2): providers of AI systems generating synthetic audio, image, video or text "shall ensure" outputs are "marked in a machine-readable format and detectable as artificially generated or manipulated", as far as technically feasible. There are exemptions for standard editing assistance and systems that do not substantially alter input. Source: https://artificialintelligenceact.eu/article/50/
- Art. 50(4): deployers of deep fakes must disclose that content is artificially generated or manipulated. For "evidently artistic, creative, satirical, fictional" works, disclosure is limited to acknowledging the content's existence "in an appropriate manner that does not hamper the display or enjoyment of the work". AI text on matters of public interest must be disclosed unless under human editorial review. Source: https://artificialintelligenceact.eu/article/50/
- Application date: 2 August 2026. Source: https://artificialintelligenceact.eu/article/50/
- Digital Omnibus on AI: reported as adopted as Regulation (EU) 2026/1744 (OJ 24 July 2026, in force 27 July 2026). It gives generative systems placed on the market before 2 August 2026 a grace period to 2 December 2026 for Art. 50(2) marking. Other Art. 50 duties apply from 2 August 2026. Sources: https://www.gibsondunn.com/eu-ai-act-omnibus-agreement-postponed-high-risk-deadlines-and-other-key-changes/ (grace period, pre-adoption) and https://www.cuatrecasas.com/en/spain/intellectual-property/art/key-aspects-ai-omnibus-regulation (adoption; regulation number not checked against EUR-Lex, so treat as secondary)
- Code of Practice on marking and labelling AI-generated content: final published 10 June 2026. Voluntary. Section 1 covers providers (machine-readable marking), Section 2 deployers (labelling). Commission guidelines on Art. 50 are to follow. Source: https://digital-strategy.ec.europa.eu/en/news/commission-publishes-code-practice-marking-and-labelling-ai-generated-content . Reported to require a layered approach (metadata, watermarking, detection) and not to mandate C2PA. Secondary: https://www.jonesday.com/en/insights/2026/06/european-commission-publishes-final-code-of-practice-on-marking-and-labelling-aigenerated-content

### California

- SB 942, the California AI Transparency Act (2024), applies to covered GenAI providers:
  - Free AI detection tool.
  - Optional manifest (visible) disclosure.
  - Latent disclosure in image, video and audio conveying provider name, system name and version, creation or alteration time, and a unique identifier.
  - $5,000 per violation.

  Sources: https://www.mofo.com/resources/insights/261001-california-revamps-ai-transparency-requirements (1 Oct 2026) and https://aicomplianceatlas.com/law/california-sb-942 (the statute page at https://leginfo.legislature.ca.gov/faces/billNavClient.xhtml?bill_id=202320240SB942 returned 403)
- AB 853 (signed 13 October 2025):
  - Moved the operative date to 2 August 2026.
  - From 1 January 2027, large online platforms must detect standards-compliant provenance data, show users origin and history, and must not knowingly strip provenance. GenAI hosting platforms have duties from the same date.
  - From 1 January 2028, capture device manufacturers must offer latent disclosures, embedded by default.

  Sources: https://www.troutmanprivacy.com/2025/10/california-ai-transparency-act-amendments-signed-into-law/ and https://www.mofo.com/resources/insights/261001-california-revamps-ai-transparency-requirements
- SB 1000 (reported enacted 30 September 2026 as an urgency statute):
  - Removes the one-million-user threshold.
  - Removes the manifest disclosure option.
  - Replaces the AI detection tool with a "disclosure verification tool".
  - Replaces licensee revocation with notice, remediation and reporting.
  - Keeps latent disclosures; one source says the disclosure must now indicate whether content was created or altered.
  - Leaves AB 853 dates unchanged.

  Source: https://www.mofo.com/resources/insights/261001-california-revamps-ai-transparency-requirements (chapter number reported as 861 by secondary trackers; not verified on leginfo)
- AB 2602 (contract terms for digital replicas, performances fixed on or after 1 January 2025) and AB 1836 (deceased performers' digital replicas) took effect 1 January 2025. The exact AB 2602 conditions are disputed between summaries. Source: https://perkinscoie.com/insights/update/single-day-california-enacts-five-bills-tackling-digital-replicas-and-deepfakes (statute text not reviewed)

### US Copyright Office

- Copyright and Artificial Intelligence report:
  - Part 1 Digital Replicas, 31 July 2024.
  - Part 2 Copyrightability, 29 January 2025.
  - Part 3 Generative AI Training, pre-publication version 9 May 2025; the final version is still "will be published in the future" per the page.

  Source: https://www.copyright.gov/ai/
- Part 2 conclusions (verbatim headings):
  - Existing law suffices.
  - Assistive AI use does not affect protection.
  - Purely AI-generated material, or material with insufficient human control, is not protected.
  - Case-by-case analysis.
  - "prompts do not alone provide sufficient control".
  - Humans are entitled to copyright in their expression "perceptible in AI-generated outputs, as well as the creative selection, coordination, or arrangement of material in the outputs, or creative modifications of the outputs."

  Source: https://www.copyright.gov/ai/Copyright-and-Artificial-Intelligence-Part-2-Copyrightability-Report.pdf
- Registration guidance (16 March 2023, 88 FR 16190): "applicants have a duty to disclose the inclusion of AI-generated content in a work submitted for registration and to provide a brief explanation of the human author's contributions." AI-generated content "more than de minimis should be explicitly excluded" (Material Excluded). Human authorship goes in the "Author Created" field. Source: https://www.govinfo.gov/content/pkg/FR-2023-03-16/html/2023-05321.htm
- Thaler v. Perlmutter: the D.C. Circuit affirmed the human authorship requirement (March 2025); the Supreme Court denied certiorari on 2 March 2026 (No. 25-449). Source: https://www.scotusblog.com/cases/case-files/thaler-v-perlmutter (secondary docket summary)

---

## 6. Existing provenance and lineage systems (positioning)

- **Tekton Chains** (Kubernetes): observes completed TaskRuns/PipelineRuns, generates SLSA v0.2 or v1.0 provenance (task- or pipeline-level), signs it, and uploads it. Inputs map to `resolvedDependencies`, outputs to `subject`. Latest releases v0.28.9 / v0.27.8 (October 2026). This is the closest prior art to "signed per-task provenance on Kubernetes", but it is software-supply-chain oriented. Sources: https://github.com/tektoncd/chains/blob/main/docs/slsa-provenance.md and https://github.com/tektoncd/chains/releases
- **in-toto Witness**: a framework that records and verifies artifact provenance as in-toto attestations; v0.12.0 2026-07-10. Source: https://github.com/in-toto/witness
- **GUAC**: aggregates supply-chain attestations into a graph database; v1.1.0 2026-03-13. Prior art for "lineage graph built from signed attestations." Source: https://github.com/guacsec/guac
- **OpenLineage / Marquez**: LF AI & Data Graduate project; open standard for run, job and dataset lineage events with extensible facets; Marquez is the reference implementation. Latest 1.54.0 (2026-10-13). Not signed or content-addressed by default. Source: https://github.com/OpenLineage/OpenLineage
- **Pachyderm**: data-versioned pipelines with built-in data provenance; last release v2.12.2 (2025-01-15). Release cadence has slowed; current status unverified. Source: https://github.com/pachyderm/pachyderm
- **OpenSSF Model Signing / model-transparency**: model integrity and signing via in-toto Statements in Sigstore bundles (see section 2). Source: https://github.com/ossf/model-signing-spec
- **ShotGrid (Autodesk; marketed as Flow Production Tracking, rename not re-verified here)**: `PublishedFile` entities carry upstream/downstream published-file links populated when a scene that references another publish is published. This is dependency tracking in a production database, not signed or content-addressed. Source: https://community.shotgridsoftware.com/t/how-to-pull-upstream-and-downstream-dependency-outside-of-the-shot-assets/14058 (community forum; official API docs not reviewed)
- **ftrack**: `AssetVersion.uses_versions` / `used_in_versions` (backed by `AssetVersionLink`) record version dependencies. Source: https://developer.ftrack.com/api-clients/examples/entity-links
- **Studio in-house pipelines (Pixar and others)**: no public documentation of provenance or lineage systems was found in this session. Unverified either way.
- **Adobe Content Authenticity app**: announced October 2024, public beta 24 April 2025. Batch signing of JPG/PNG, a generative-AI training preference, and LinkedIn verified identity. GA status as of October 2026 unverified. Source: https://blog.adobe.com/en/publish/2025/04/24/adobe-content-authenticity-now-public-beta-helps-creators-secure-attribution
- **Cameras and devices**:
  - Leica M11-P (late October 2023) is described as the first camera with Content Credentials. Source: https://contentauthenticity.org/blog/leica-launches-worlds-first-camera-with-content-credentials
  - Sony supports C2PA signing for stills and, on the PXW-Z300 camcorder (separate licence), for video. Sony's free default certificate "does not guarantee the validity of the C2PA signature". Source: https://pro.sony/en_BE/authenticity
  - Nikon Z6III C2PA (firmware 2.00) was suspended in September 2025 after a signing flaw, and all issued certificates were revoked. Reported still suspended in early 2026. Source: https://petapixel.com/2025/09/22/nikon-cant-fully-solve-the-z6-iiis-c2pa-problems-alone/
  - Google Pixel 10 Camera is reported at C2PA Assurance Level 2. Source: https://blog.google/security/pixel-android-trusted-images-c2pa-content-credentials/
- **"Human Provenance in Film"**: an AI disclosure taxonomy from The Mise En Scène Company, launched at the Cannes market on 12 May 2026, in consultation through 31 October 2026. Source: https://rightstech.com/2026/05/new-ai-disclosure-standard-for-film-launched-at-cannes-film-market/ (secondary)
- **SDKs**: the C2PA Rust SDK `c2pa-rs` is at v0.91.2 (2026-10-06); still pre-1.0. Source: https://github.com/contentauth/c2pa-rs/releases

---

## Implications for the design

1. **Record format.** Emit in-toto Statement v1 (`_type: https://in-toto.io/Statement/v1`) in a DSSE envelope with `payloadType: application/vnd.in-toto+json`. Define a versioned c4-operator predicate (for example `https://<c4-operator domain>/provenance/task/v1`) that resolves to a human-readable spec. For tasks that build container images or software, also emit the standard `https://slsa.dev/provenance/v1` predicate so existing verifiers (slsa-verifier, Tekton-style policy) work unchanged.

2. **Digest keys for file subjects.** A C4 file ID is SHA-512 of the bytes per ST 2114. Put `"sha512": <lowercase hex>` in every file subject's DigestSet so generic in-toto tooling can match it. Add `"c4": <90-char C4 ID>` as a second entry, and document its encoding (ST 2114 base58 with `c4` prefix) in the predicate spec, as the DigestSet rules require. Consider also adding `sha256` for artifacts that other ecosystems name that way (OCI images, OMS model manifests).

3. **Digest keys for tree subjects.** Never label a C4 tree ID as `sha512`: it is not the SHA-512 of a single byte stream, so that would be a false claim. Use the `c4` key with a documented definition of the tree computation, and record which metadata projection was used (the `c4` CLI tree ID changes with modes and timestamps unless projected away, for example `-m c`). Accept that generic verifiers will ignore tree subjects. Where interop matters, also list member files as subjects or use `gitTree` only if actually computed.

4. **SLSA claims.** Map task fields onto the SLSA provenance shape:
   - `externalParameters`: submitter-set parameters, which must be complete to claim Build L3.
   - `resolvedDependencies`: input trees, image digests, model weights.
   - `internalParameters`: node class, scheduler decisions.
   - `builder.id`: per-cluster operator URI.

   Sign in the operator control plane with keys unreachable from task pods; that is the Build L2/L3 requirement. Claim "SLSA Build L3-style provenance for compute tasks", not "SLSA compliance for media": SLSA v1.2 has no data or media track.

5. **Generative and ML records.** Do not invent a dataset standard claim; none exists in in-toto or SLSA. Record model weights, LoRAs and training-data trees as `resolvedDependencies` with c4 plus sha256 digests. Where an OMS signature bundle exists for a model, record and verify it (OMS v1.x is an in-toto Statement in a Sigstore bundle). Store prompts as hashed parameters, with the plaintext access-controlled, because prompts can be confidential script material under the WGA terms.

6. **C2PA export scope.**
   - Embed: MOV (including ProRes in MOV) and MP4 via BMFF hashing; WAV/BWF; PDF.
   - Sidecar `.c2pa` or a manifest repository: MXF/IMF and EXR sequences. For EXR sequences, use `c2pa.hash.collection.data` over the frames.
   - Do not claim C2PA "support" for EXR or MXF beyond a spec-sanctioned sidecar until OpenEXR and SMPTE standardize carriage.

   Results show as "Trusted" only with a certificate on the C2PA Trust List, which means going through the conformance program (54 products as of May 2026). Until then, outputs are "Valid", not "Trusted". Timestamp signatures via a TSA on the C2PA TSA Trust List.

7. **AI labels in C2PA output.**
   - Fully generated media: `c2pa.created` with `digitalSourceType` `trainedAlgorithmicMedia`.
   - Comps containing generated elements: `compositeWithTrainedAlgorithmicMedia`, and `c2pa.placed` for inserted elements.
   - The 2.4 `c2pa.ai-disclosure` assertion (`modelType`, `modelIdentifier`, `humanOversightLevel`).
   - Training permissions: `cawg.training-mining` (CAWG v1.1), not the removed `c2pa.training-mining`.
   - Derive all of these from the lineage graph rather than asking artists to set them by hand.

8. **Disclosure queries the graph must answer** (each maps to a cited obligation):
   - WGA 2023: was any material given to a writer AI-generated?
   - WGA 2026 and DGA 2026: was any script or footage licensed to train a commercial GAI system? This is a notice trigger.
   - SAG-AFTRA: does an output contain a digital replica of an identifiable performer? Is it employment-based or independently created, and which consent record covers it? Was a synthetic performer used? That triggers union notice, plus the 2026 "significant additional value" justification.
   - IATSE: which covered employees used which AI systems on which tasks, and was use disclosed to the Producer? This is the indemnity condition.
   - DGA 2026: which AI-generated footage exists, and who approved it (director control)?
   - Netflix-style policy: list all GenAI uses per title, flag on-screen GenAI in final deliverables, and show the written approval reference for key characters, settings or performance alterations.

9. **Human authorship records.** For copyright registration (duty to disclose AI content and describe human contribution; exclude AI material that is more than de minimis), record per output:
   - human Participants and Roles;
   - human-authored inputs (plates, paintovers, reference) as separate subjects;
   - human selection among generated candidates;
   - subsequent human modifications as distinct tasks.

   Prompts alone do not establish authorship (USCO Part 2), so the graph must keep prompt-only steps distinct from human expressive input. Do not claim the system "proves" copyrightability. It supplies evidence for a case-by-case determination.

10. **Regulatory export.** Under EU Art. 50(2), the AI provider must mark outputs; this applies from 2 August 2026, with a grace period to 2 December 2026 for systems already on the market. Under California SB 942 as amended, the latent disclosure carries provider, system name and version, creation time and unique ID. A studio that runs its own generative models may itself be the "provider". Make the latent-disclosure fields derivable from task records (model ID and version, operator identity, timestamp, record ID). Treat legal applicability as counsel's call. The Art. 50(4) deep fake labelling for artistic works is a deployer-side, light-touch duty, not something the operator does automatically.

11. **Interoperability with production systems.** Express the graph as OMC on export:
    - Task to `Task`, submitter to `Participant`, node class to `Infrastructure`.
    - `provenance.CreatedBy` / `createdOn` / `Role` / `Origin` / `reason`.
    - `version.DerivationOf` / `RevisionOf` / `VariantOf`.
    - C4 IDs as `identifier` with `identifierScope: "c4"`.

    Feed back into ShotGrid/Flow `PublishedFile` links and ftrack `uses_versions`. This positions c4-operator as the signed, content-addressed layer under those trackers, not a replacement for them.

12. **Honest positioning.** Tekton Chains already does signed SLSA provenance per task on Kubernetes. Witness and GUAC do attestation collection and graphs. OpenLineage/Marquez does job and dataset lineage. ShotGrid and ftrack do publish dependencies. Cameras (Leica, Sony, Pixel) do capture-time C2PA. The defensible differentiator is the combination: content-addressed tree IDs (ST 2114) as subjects across render, ML and generative tasks; production context (project, submitter, node class) in signed records; and contract-specific disclosure queries. Avoid "first" or "only" claims, and note the Nikon revocation as evidence that signing infrastructure must plan for key compromise and certificate revocation.
