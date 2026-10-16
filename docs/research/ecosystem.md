# c4-operator ecosystem brief: clouds, marketplaces, render licensing, metering

Research date: 2026-10-16. Every claim carries a source URL. "Unverified" means I could not confirm it from a primary source. Prices are list prices as published on the date noted.

---

## 1. Co-sell and marketplace programs

### AWS

**ISV Accelerate (ISVA).** AWS's co-sell program for software partners. Requirements as stated on the program page: one or more software products listed as generally available in AWS Marketplace; ACE program eligibility; "Validated or Differentiated status in AWS Partner Central"; Amazon Payee Central set up; at least 5 launched opportunities (ACE or Marketplace private offers) and at least 15 qualified ACE opportunities in the past 12 months (FVO excluded); at least one person who has completed the "Co-Selling with AWS" learning module; and at least $2,000 in recognized AWS account revenue at enrollment. Enrollment runs through the AWS Partner Network Software Path. Benefits listed: visibility with AWS sales, co-sell support, ISV funding and tools.
Source: https://aws.amazon.com/partners/programs/isv-accelerate/

- An APN blog says a funding benefit applies to partners newly enrolled after 2026-01-01 who implement Partner Revenue Measurement. Source: https://aws.amazon.com/blogs/apn/new-aws-isv-accelerate-benefits-unlock-the-co-sell-advantage/ (exact MDF terms are in Partner Central, login required; unverified).
- Third-party guides say the Foundational Technical Review (FTR) is the step before Software Path validation. Source: https://help.clazar.io/articles/8525930522-aws-isv-accelerate-program-guide (secondary).

**"Deployed on AWS" badge.** Since 2025-05-01 AWS Marketplace accepts SaaS products hosted anywhere; products 100% deployed on AWS get a "Deployed on AWS" badge and "continue to count toward additional customer benefits." Source: https://aws.amazon.com/about-aws/whats-new/2025/05/aws-marketplace-saas-products-deployment-locations/ . The SaaS guidelines require the application and control planes to run on AWS for the badge. Source: https://docs.aws.amazon.com/marketplace/latest/userguide/saas-guidelines.html . The claim that only badged spend draws down a customer's private pricing commitment comes from third-party sources (https://clazar.io/blog/aws-new-saas-policy); treat as unverified. The badge as documented is a SaaS-listing concept; I found no equivalent badge for container (Helm) listings, which by definition run in the buyer's AWS account.

**Container products (Helm) and metering.** AWS Marketplace container products can be priced three ways:
- Hourly per task or pod, metered by AWS automatically: software calls `RegisterUsage` at launch.
- Custom units the seller defines: software calls `MeterUsage`.
- Contract pricing: entitlements delivered through AWS License Manager; "License Manager doesn't do any tracking or updates; this is done by the seller's application."
Source: https://docs.aws.amazon.com/marketplace/latest/userguide/container-products-billing-integration.html

Details from the same page that matter for design:
- Credentials come from the buyer's IAM role for the pod (IRSA on EKS); do not embed credentials.
- Calls outside AWS container services return `PlatformNotSupportedException`; supported targets are ECS, EKS, and Fargate.
- The Region must be discovered at runtime.
- If product codes are overridable through Helm values, the software must keep a list of trusted product codes so a free code cannot substitute for a paid one.
- A buyer who unsubscribes may keep running existing containers but cannot launch new ones.

`RegisterUsage` should be called at launch; AWS gives no metering guarantee for earlier months if a container is not registered within 6 hours. Source: https://docs.aws.amazon.com/marketplace/latest/APIReference/API_marketplace-metering_RegisterUsage.html

**AWS Marketplace for Containers Anywhere.** Lets buyers deploy Marketplace container products on EKS Anywhere, ECS Anywhere, EC2, or on-premises clusters. Billing uses contract pricing through License Manager (dimensions such as Hosts, Requests, Users, Units; up to 24 dimensions), not hourly metering.
Sources: https://aws.amazon.com/blogs/aws/new-aws-marketplace-for-containers-anywhere-to-deploy-your-kubernetes-cluster-in-any-environment ; https://docs.aws.amazon.com/marketplace/latest/userguide/container-anywhere-license-manager-integration.html

### Google Cloud

**Partner program name.** Google announced the **Google Cloud Partner Network (GCPN)** on 2025-12-16. It rolls out in Q1 2026 with a six-month transition window. Tiers are Select, Premier, and a new Diamond; specializations are replaced by competencies based on "capacity" and "capability" (validated closed/won opportunities); the program rewards "successful co-sell sales efforts" and "shared innovation with ISVs." The Partner Network Hub replaces the old portal. Source: https://cloud.google.com/blog/topics/partners/introducing-google-cloud-partner-network . Google's post does not say in words that GCPN replaces **Partner Advantage**; third-party sources say it does (https://flashdba.com/hyperscaler-gtm/co-sell/google-cloud/). Cite "Google Cloud Partner Network (formerly Partner Advantage)" with that caveat. ISV-specific qualification thresholds under GCPN: unverified.

**Kubernetes apps on Google Cloud Marketplace.** Packaging requirements:
- A deployer container image and an Application custom resource.
- Helm charts must work with `--client-only`.
- Commercial (non-BYOL) apps "must report its usage to Google," with the Usage-based Billing Agent recommended.
- Marketplace creates a reporting Secret (`entitlement-id`, `consumer-id`, `reporting-key`) and passes its name in via `REPORTING_SECRET`.
- Target clusters are "GKE or GKE Enterprise" on x86 nodes.
Source: https://docs.cloud.google.com/marketplace/docs/partners/kubernetes/create-app-package

- Pricing models are free, BYOL, or usage-based on vendor-chosen metrics. Units are integers by default and custom metrics are deltas. Pricing review takes up to four business days; price changes need 30 days after approval. Source: https://docs.cloud.google.com/marketplace/docs/partners/kubernetes/select-pricing
- The Usage-based Billing Agent (`ubbagent`) runs as a sidecar. It aggregates and persists reports and forwards them to Google Service Control. Source: https://github.com/GoogleCloudPlatform/ubbagent

**Commit drawdown.** Google says customers can "decrement their committed spend" through Marketplace purchases of third-party solutions. Source: https://cloud.google.com/blog/topics/partners/announcing-google-cloud-marketplace-private-offers . A 2025 channel report puts channel private offers at 100% counting toward commitment, capped at 25% of the commitment. Source: https://www.channele2e.com/news/google-cloud-marketplace-introduces-new-revenue-models-and-incentives-to-boost-partner-growth (secondary; current cap unverified).

### Microsoft

**Program names.** The membership program is the **Microsoft AI Cloud Partner Program**. Source: https://learn.microsoft.com/en-us/partner-center/referrals/co-sell-requirements

On 2026-09-28 Microsoft announced **Frontier Accelerate for Marketplace**, a staged ISV program with three stages (Build & Publish, Grow, Differentiate). It has a no-cost option and a paid Premium option; new Premium partners get up to $30,000 in Azure dev/test credits. Eligibility requires active AI Cloud Partner Program membership, a repeatable B2B solution built on Microsoft Cloud, and intent to sell through Marketplace. ISV Success participants are auto-enrolled in Premium. Marketplace Rewards users, Azure IP co-sell partners, and certified software designation holders get the no-cost option.
Source: https://www.channelinsider.com/channel-business/news-microsoft-frontier-accelerate-marketplace-sales-publishers/

The Microsoft co-sell page (updated 2026-10-05) already references Frontier Accelerate for Business Applications ISVs. Source: https://learn.microsoft.com/en-us/partner-center/referrals/co-sell-requirements

**Co-sell statuses and Azure IP co-sell eligible.** Source for this subsection: https://learn.microsoft.com/en-us/partner-center/referrals/co-sell-requirements

*Co-sell ready* requires:
- A PartnerID and a Marketplace account.
- A complete business profile.
- A live Marketplace offer.
- A sales contact per geography.
- Co-sell collateral (a one-pager and a pitch deck).

*Azure IP co-sell eligible* applies to Azure Application, **Azure Container**, Azure VM, and SaaS offers whose solution type is "IP." It adds four requirements:
1. At least USD 100,000 of Azure Consumed Revenue or Marketplace Billed Sales over the trailing 12 months, at the organization level. Azure credits do not count.
2. Microsoft technical validation that the solution is "primarily platformed in Azure." The offer must be "actively platformed on Microsoft Azure at the time of the transaction."
3. A reference architecture diagram (not mandatory for Containers).
4. The offer must be transactable on Marketplace.

**MACC.** Source for this subsection: https://learn.microsoft.com/en-us/partner-center/marketplace-offers/azure-consumption-commitment-enrollment
- An offer is enrolled in MACC (Microsoft Azure Consumption Commitment) once it meets all Azure IP co-sell requirements, typically within a week.
- Free and BYOL offers are not transactable and cannot qualify.
- Enrollment is per offer.
- "The MACC benefit only applies to licenses that are exclusively used in Azure"; hybrid or on-prem use disqualifies.

**Kubernetes application offers (Azure Container offer type).**

Packaging (source: https://learn.microsoft.com/en-us/partner-center/marketplace-offers/azure-container-technical-assets-kubernetes):
- The app must be Helm-based with all images referenced in the chart; nothing may be pulled at runtime.
- It is packaged as a CNAB (Helm chart, createUiDefinition, ARM template, manifest).
- The CNAB is copied into a Microsoft-owned **public** ACR. Images there "are accessible to anyone."
- Linux AMD64 images only.
- Targets are managed AKS or Arc-enabled Kubernetes; a single offer targets one type.
- **Arc-enabled offers support only pre-defined billing models** (no custom meters).
- Per-core, per-pod, and per-node billing require the pod label `azure-extensions-usage-release-identifier`. Custom meters take `clientId`, `planId`, and `resourceId` through `values.yaml`.
- The app is delivered as an AKS cluster extension.

Pricing options (source: https://learn.microsoft.com/en-us/partner-center/marketplace-offers/azure-container-plan-availability):
- BYOL.
- Per core, per every core in cluster, per node, per every node in cluster, per pod, per cluster. These are reported hourly and billed monthly.
- Custom meters, where "your application will have to emit usage events."

### Oracle

**OCI Marketplace / Oracle PartnerNetwork (OPN).**
- The relevant OPN track is the **Cloud Build Track** for "commercially available software ... built on and optionally integrated with Oracle Cloud." It lists a $500 OPN membership fee and a $3,000 track fee; Marketplace publishing is a membership/track enabler; OPN Expertise designations add go-to-market benefits. Source: https://www.oracle.com/partnernetwork/program/build/ (page returned 403 to my fetcher; content taken from the search index; verify fees).
- Pricing models are free, BYOL, paid (hourly, billed by Oracle against the customer's Universal Credits), and private offers. Customer spend on third-party listings is capped at 25% of active Universal Commit Credit. Source: https://docs.oracle.com/en-us/iaas/Content/Marketplace/pricing-models.htm
- **Helm charts and container images cannot be paid listings:** "Currently, only free and BYOL pricing models are available for listings with the container or helm chart artifacts." Paid hourly listings must be images (or stacks referencing a paid image), billed $/OCPU-hour or $/instance-hour. Source: https://docs.oracle.com/en-us/iaas/Content/Marketplace/managing-images.htm
- Paid listings require a US entity. Source: https://docs.oracle.com/en-us/iaas/Content/partner-portal/partner-portal_gs-register_as_a_marketplace_publisher.htm
- Oracle has cut publisher fees on paid transactions from 20% to 3%. Source: https://blogs.oracle.com/cloud-infrastructure/oracle-cloud-marketplace-drops-publishing-fees-from-20-to-3-for-all-paid-publisher-listing-transactions (effective date unverified).
- Oracle co-sell program terms for ISVs: unverified.

### CoreWeave

- CoreWeave launched the **CoreWeave Partner Network** on 2026-09-30. It is a curated set of partners in four categories (infrastructure, data services, ISVs, models and inference) whose integrations are "tested on CoreWeave under production load." Named ISVs include Datadog, Grafana Labs, Pulumi, and Zscaler. Joining is by "Contact us." Sources: https://www.coreweave.com/coreweave-partner-network ; https://www.coreweave.com/news/coreweave-partner-network-accelerates-the-path-to-ai-innovation
- CoreWeave's blog says partners are selected when customers already run them or ask for them, and that the network is not exclusive. Source: https://www.coreweave.com/blog/coreweave-partner-network-jumpstarts-your-ai-stack
- **No CoreWeave marketplace, no billing-through-CoreWeave, and no co-sell terms are documented.** The partner page describes none of these.

---

## 2. Kubernetes, GPU capacity, object storage, KMS

### Burst autoscaling

| Provider | Mechanism | Notes |
|----------|-----------|-------|
| AWS EKS | Karpenter (self-managed) or EKS Auto Mode (managed Karpenter) | Both use `NodePool` (karpenter.sh/v1). Auto Mode uses `NodeClass` (eks.amazonaws.com/v1); self-managed uses `EC2NodeClass`. Both support On-Demand, Spot, Capacity Blocks, and ODCRs. Auto Mode handles GPU drivers and Spot interruption natively; nodes are Bottlerocket with a 21-day maximum lifetime. https://docs.aws.amazon.com/eks/latest/userguide/ml-node-pools.html |
| Google GKE | Node auto-provisioning plus custom ComputeClasses | A ComputeClass can prefer Spot and fall back to on-demand through an ordered `priorities` list, with `nodePoolAutoCreation`. It can select GPUs/TPUs and be set as the cluster default. https://docs.cloud.google.com/kubernetes-engine/docs/concepts/about-compute-classes |
| Azure AKS | Node auto-provisioning (NAP) | NAP "automatically deploys, configures, and manages Karpenter" and is based on Karpenter and the open-source AKS Karpenter provider. It uses `NodePool` and `AKSNodeClass` CRDs, is preconfigured in AKS Automatic, and is opt-in on Standard. Limits: no Windows node pools, no IPv6, no service principals, cluster cannot be stopped. https://learn.microsoft.com/en-us/azure/aks/node-auto-provisioning . Azure Updates lists GA as July 2025: https://mc.merill.net/message/AZ498247 (mirror of Microsoft message center). |
| CoreWeave CKS | Upstream Cluster Autoscaler on Node Pools | Requires Kubernetes 1.32+. Bare-metal scale-up takes 5 to 15 minutes. `maxNodes` must fit within quota. Not supported on SUNK or on rack-scale types (GB200, GB300). No charge while nodes scale up. The page does not mention spot or interruptible capacity. https://docs.coreweave.com/products/cks/nodes/autoscaling |
| Oracle OKE | Cluster Autoscaler, or Karpenter Provider for OCI (KPO) | Oracle announced KPO GA and open source. It maps Karpenter's spot capacity type to OCI preemptible instances and supports capacity reservations and compute clusters. https://blogs.oracle.com/cloud-infrastructure/smarter-kubernetes-autoscaling-with-karpenter (403 to my fetcher; via search index) |

Design note: four of five providers now expose the Karpenter `NodePool` API (EKS, AKS NAP, OKE KPO, and EKS Auto Mode). The provider-specific parts are the NodeClass kinds and label prefixes. GKE uses ComputeClass and CoreWeave uses Cluster Autoscaler node pools.

### Object storage and S3 compatibility

- **AWS S3:** native.
- **Google Cloud Storage:** the XML API accepts S3 tools. Point the endpoint at `https://storage.googleapis.com`, use HMAC keys in place of AWS keys, and note that only `x-amz-` headers with `x-goog-` equivalents are honored. Source: https://docs.cloud.google.com/storage/docs/aws-simple-migration
- **Azure Blob Storage:** **no native S3 API found** as of 2026-10-16. S3 access needs a gateway (for example S3Proxy, Flexify, or MinIO). Sources: https://flexify.io/how-to-run-amazon-s3-apps-on-azure ; https://learn.microsoft.com/en-us/answers/a/1431939 . Microsoft Ignite runs 2026-11-17 to 20, so check again after it. Source: https://azure.microsoft.com/en-us/blog/azure-source-volume-79/
- **CoreWeave AI Object Storage:** S3-compatible. The primary endpoint is `https://cwobject.com`. In-cluster, the LOTA caching endpoint is `http://cwlota.com`, which caches GETs by default. **Virtual-hosted addressing is required; path-style is not supported.** Source: https://docs.coreweave.com/docs/products/storage/object-storage/lota/about . SSE-C has been available since 2025-08-04. Source: https://docs.coreweave.com/docs/changelog/release-notes/ai-object-storage-sse-c
- **OCI Object Storage:** has an Amazon S3 Compatibility API using Customer Secret Keys. Each user can hold up to two keys, the secret is Oracle-generated and cannot be retrieved later, and the keys inherit the user's permissions. Data is interchangeable with the native API. Sources: https://docs.oracle.com/iaas/Content/Object/Tasks/s3compatibleapi.htm ; https://docs.oracle.com/en-us/iaas/tools/oci-cli/3.55.0/oci_cli_docs/cmdref/iam/customer-secret-key.html

### KMS and secrets encryption

- **AWS:** EKS envelope-encrypts all Kubernetes API data by default on 1.28+ (KMS v2). Keys are AWS-owned unless you configure a customer-managed KMS key. Source: https://docs.aws.amazon.com/eks/latest/userguide/envelope-encryption.html
- **Google:** GKE application-layer secrets encryption uses a Cloud KMS key-encryption key. Source: https://docs.cloud.google.com/kubernetes-engine/docs/how-to/encrypting-secrets
- **Azure:** AKS KMS etcd encryption uses Azure Key Vault. Secrets created before enablement stay unencrypted until rewritten. Source: https://learn.microsoft.com/en-us/azure/aks/use-kms-etcd-encryption
- **Oracle:** OKE encrypts etcd by default with an Oracle-managed key, or optionally with an OCI Vault master key (`--kms-key-id`). If that key is deleted, the cluster's secrets become permanently inaccessible. Source: https://docs.oracle.com/iaas/Content/ContEng/Tasks/contengencryptingdata.htm
- **CoreWeave:** no general-purpose KMS product found. SSE-C exists for object storage (above). "Remote Key Encryption" (client-side, customer-held keys; HashiCorp Vault Enterprise and KMIP at launch; limited availability later in 2026) was reported by SiliconANGLE on 2026-09-22. Source: https://siliconangle.com/?p=847547 (not yet in CoreWeave docs; unverified).

Cloud-neutral design implications from section 2:
- Azure is the outlier on S3.
- CoreWeave rejects path-style S3 addressing.
- GKE and CoreWeave do not use Karpenter.
- Key management differs per provider, and CoreWeave has no KMS. A neutral design needs a pluggable KMS interface with a Vault/KMIP option.

---

## 3. Usage-based licensing (UBL) for render software

### Legacy Thinkbox UBL (retired)

How it worked (source: https://docs.thinkboxsoftware.com/products/deadline/10.3/1_User%20Manual/manual/licensing-usage-based.html):
- Thinkbox hosted a Cloud License Server.
- An on-premises **Deadline License Forwarder** "forwards license requests for 3rd Party products to Thinkbox's Cloud-hosted License Server Gateway," using third-party certificates from Thinkbox.
- Render time was **prepaid in bundles on the Thinkbox Marketplace**.
- Third-party time was "consumed per minute and only while Deadline is rendering a task." Workers checked out time in 5-minute increments and returned unused minutes on exit.
- Deadline **Limits** capped concurrent use at the floating-license count. Workers beyond the cap ran in "Overage" mode against UBL; setting a Limit to zero forced all-UBL.

Retirement: the Thinkbox UBL Marketplace stopped selling everything except V-Ray and RenderMan on 2025-04-30 and stopped those on 2025-08-15. Legacy UBL "is no longer available as of September 30, 2025." Source: https://docs.thinkboxsoftware.com/products/deadline/10.4/1_User%20Manual/manual/aws-portal/licensing-overview.html . The License Forwarder docs describe it as part of "Legacy Third Party Usage-Based Licensing." Source: https://docs.thinkboxsoftware.com/products/deadline/10.1/1_User%20Manual/manual/license-forwarder.html

### Deadline 10 status

Deadline 10 entered **maintenance mode on 2025-11-07**: "security updates and critical fixes rather than new features." Existing deployments keep working, and AWS Portal, Spot Event Plugin, and RFDK are still usable. DCC integration updates continue. UBL for Deadline 10 users "is accessed through Deadline Cloud UBL." No end-of-life date is given. Source: https://docs.thinkboxsoftware.com/products/deadline/10.4/1_User%20Manual/manual/maintenance-mode-faq.html . AWS recommends Deadline Cloud for new work. Source: https://aws.amazon.com/media-services/thinkbox/

### AWS Deadline Cloud UBL today

**Mechanism.**
- Service-managed fleets (SMF) are licensed automatically.
- Customer-managed fleets (CMF) need a **license endpoint**. Creating one provisions an AWS PrivateLink interface endpoint in the customer VPC (billed at PrivateLink rates). Applications point at it as if it were a normal license server.
- Quotas: 5 license endpoints per Region and 500 license sessions per endpoint.
- UBL "provides licenses for your Deadline Cloud workers to render, it doesn't provide licenses for your DCC applications."
Sources: https://docs.aws.amazon.com/deadline-cloud/latest/developerguide/cmf-ubl.html ; https://docs.aws.amazon.com/deadline-cloud/latest/developerguide/license.html

**Products supported on CMF, with ports and client configuration** (all from cmf-ubl.html above):

| Product | Port(s) | Client configuration |
|---------|---------|----------------------|
| Arnold | 2701 | `ADSKFLEX_LICENSE_FILE=2701@<endpoint>` (FlexNet-style) |
| Cinema 4D | 7057 | `g_licenseServerRLM` |
| Nuke | 6101 | `foundry_LICENSE=6101@<endpoint>` (RLM-style) |
| Red Giant | 7055 | `redshift_LICENSE` |
| Redshift | 7054 | `redshift_LICENSE` |
| Houdini, Mantra, Karma | 1715 to 1717 | `hserver -S "http://<endpoint>:1715;..."` |
| V-Ray | 30304 | `VRAY_AUTH_CLIENT_SETTINGS=licset://<endpoint>:30304` |

The doc says these "are the only applications supported with usage-based licensing on customer-managed fleets." Maya and 3ds Max UBL are therefore SMF-only.

**Current per-hour prices.** Taken from the AWS pricing data feed behind https://aws.amazon.com/deadline-cloud/pricing/ (file `pricing/2.0/meteredUnitMaps/deadline/USD/current/deadline.json`, publication date 2026-10-06). Values are identical in us-east-1 and eu-west-2.

| Vendor | Product | USD per hour |
|--------|---------|--------------|
| Autodesk | Maya | 0.18 |
| Autodesk | Arnold | 0.66 |
| Autodesk | 3ds Max | 0.18 |
| Foundry | Nuke | 0.76 (0 to 60 h), 0.66 (60 to 350), 0.57 (350 to 800), 0.46 (800 to 10,000), 0.38 (10,000 to 30,000), 0.33 (30,000+) |
| SideFX | Houdini | 0.75 (0 to 600 h), 0.60, 0.2625, 0.2125, 0.15, 0.13125, 0.0825, 0.05125, down to 0.03 (4,000,000+ h) |
| SideFX | Karma | 0.1875 (0 to 600 h), 0.15, 0.06625, 0.05375, 0.0375, 0.0325, 0.02125, 0.0125, down to 0.0075 (4,000,000+ h) |
| SideFX | Mantra | 0.00 |
| Maxon | Cinema 4D | 0.12 on SMF, 0.25 on CMF |
| Maxon | Redshift | 0.50 on SMF, 1.00 on CMF |
| Maxon | Red Giant | 0.50 on SMF, 1.00 on CMF |
| Chaos | V-Ray | 0.6125 |

- The pricing page's own worked example bills Arnold at "$0.66 per hour" for 200 hours, which matches the feed. Source: https://aws.amazon.com/deadline-cloud/pricing/
- Billing granularity (per minute or per hour) is not stated on the page; unverified.
- **KeyShot and After Effects:** not on the UBL price list; Deadline Cloud UBL support for them is unverified. After Effects appears in the docs only as the host for Red Giant.
- **Blender:** a Deadline Cloud blog lists Blender among the conda packages shipped to SMF workers. It has no UBL line because it is GPL and free. Source: https://aws.amazon.com/blogs/media/create-a-conda-package-and-channel-for-aws-deadline-cloud
- CMF connected-worker hours are $0.015 per worker-hour (same pricing feed).

### Other per-hour or cloud licensing for media software

- **SideFX:** the cloud FAQ recommends a remote license server or SideFX-hosted login-based licensing. A self-run license server in the cloud "cannot serve over a public-facing interface" and needs a private network or VPN. Licenses installed directly on an instance must be returned before termination. Named providers: Gridmarkets, AWS Deadline Cloud, Conductor, Drop & Render. Source: https://www.sidefx.com/faq/question/cloud . A 2017 SideFX post made Houdini Engine floating licenses "Global Access" (usable in the cloud) and listed weekly and monthly rentals. Source: https://www.sidefx.com/community/more-flexible-houdini-engine/ (current prices unverified).
- **Chaos:** runs a **Chaos Authorized Render Farm** partner program. Farms are "licensed directly by Chaos" under a Render Farm Service Agreement (RFSA) after applying; the page lists no specific requirements. Source: https://www.chaos.com/render-farms/become-partner . Chaos Cloud uses Chaos Credits, now shared with AI features. Source: https://www.chaos.com/get-more . A public per-hour license price outside Deadline Cloud: unverified.
- **Foundry:** farm nodes use `nuke_r` render licenses served by RLM. Source: https://support.foundry.com/hc/en-us/articles/207823889 . NukeX and Nuke Studio subscriptions include two Nuke Render subscriptions. Source: https://foundry.com/news-and-awards/foundry-launches-annual-subscription-nuke-products . Foundry's per-hour cloud program outside Deadline Cloud: unverified.
- **Autodesk Flex:** prepaid tokens with **daily** (24-hour) consumption per product, not hourly. Flex covers Arnold access. Source: https://www.autodesk.com/sg/benefits/flex . Autodesk's render-farm partner terms: unverified.
- **Maxon:** UBL through Deadline Cloud, as above. A separate Maxon farm program: unverified.

---

## 4. License servers: RLM and FlexNet Publisher

### RLM (Reprise)

**Architecture.** The license server is the generic `rlm` process plus one ISV server per vendor. `rlm` "handles requests from clients and directs them to the appropriate ISV server." Source: https://docs.reprisesoftware.com/docs/admin/the-license-server.html

**Checkout flow.** Clients find the server through `RLM_LICENSE` (or a vendor-specific variable such as `foundry_LICENSE`) in `port@host` form. They connect to `rlm`, then "connect to the ISV server and send a 'check out' request." Both the `rlm` port and the ISV port must be reachable. Source: https://docs.reprisesoftware.com/docs/admin/installing-rlm-licensed-products.html

**Ports and timeouts.**
- The default `rlm` port is 5053 (also UDP 5053 for broadcast discovery).
- The web admin port is 5054.
- ISV ports are random unless pinned with `port=` on the ISV line.
- Client connect timeout defaults to 10 s and read/write to 5 s.
Sources: https://docs.reprisesoftware.com/docs/admin/the-license-server.html ; https://www.starnet.com/help/license_server/

**Admin controls.** The ISV options file supports RESERVE, INCLUDE/EXCLUDE (by user, host, IP, or group), MAX per user/group, and REPORTLOG/DEBUGLOG. Source: https://docs.reprisesoftware.com/docs/admin/isv-options-file.html

**Cloud.** Reprise steers vendors to **RLM Cloud**, a hosted license server farm of standard RLM servers. It cites "Issues related to license servers running on virtual machines" as a problem that RLM Cloud avoids. Sources: https://docs.reprisesoftware.com/docs/isv/cloud-computing.html ; https://reprisesoftware.com/post/put-your-license-servers-in-the-cloud-with-rlm-cloud

### FlexNet Publisher (FlexLM)

**Architecture and ports.** `lmgrd` listens on 27000 to 27009 by default and hands the client to a vendor daemon, which picks a random port unless `PORT=` is set on the VENDOR/DAEMON line. Both ports must be open through firewalls. Sources: https://community.flexera.com/s/article/what-are-the-default-tcp-ports-for-components-of-the-flexnet-publisher-license-server-lmgrd-and-vendor-daemon ; https://community.revenera.com/s/article/configuring-flexnet-publisher-to-work-through-a-firewall

**Options file.** INCLUDE/EXCLUDE (user, host, group), MAX, REPORTLOG, and borrowing controls (INCLUDE_BORROW, EXCLUDE_BORROW). Sources: https://www.ni.com/r/flexnetpuboptions ; https://www.autodesk.com/support/download-install/admins/network-licenses/about-licensing-options-and-tools/configure-an-options-file

**Usage-based options.** Revenera's FlexNet Operations supports metered (usage-based) license models with usage capture uploaded from license servers, and a hosted Cloud Licensing Service. Source: https://docs.revenera.com/fno_CURRENT/producer/Content/helplibrary/opsGS_UM_MeteredLics.htm

### Proxies and forwarders known to work

- **AWS Deadline Cloud license endpoints** serve FlexNet-style (Arnold), RLM-style (Nuke, Cinema 4D, Redshift), SideFX hserver, and Chaos licset clients over a PrivateLink endpoint. This is vendor-sanctioned forwarding in production. Source: https://docs.aws.amazon.com/deadline-cloud/latest/developerguide/cmf-ubl.html
- **Thinkbox License Forwarder** did the same for legacy UBL, using vendor certificates issued by Thinkbox. Retired. Source: https://docs.thinkboxsoftware.com/products/deadline/10.1/1_User%20Manual/manual/license-forwarder.html
- **Plain TCP forwarding** of a customer-owned server (SSH tunnel, VPN, Kubernetes Service in front of pinned ports) is what the firewall guidance above implies. SideFX explicitly requires private networking for cloud license servers.

### What a broker can and cannot do

**Feasible without vendor cooperation:**
- Gate which pods may reach a license server (NetworkPolicy, per-pod env injection of `port@host`).
- Enforce concurrency with the existing options-file MAX/RESERVE/INCLUDE rules and scheduler-side limits, as Deadline Limits did.
- Measure per-task usage by correlating pod lifetimes with REPORTLOG/debug-log checkouts.
- Route overflow to a vendor-sanctioned UBL endpoint (Deadline Cloud) where the customer has one.

**Requires vendor cooperation:**
- Minting or signing licenses.
- Issuing per-task short-lived grants that the vendor's own client honors. The client trusts only the vendor daemon and its signed license file.
- Reselling vendor hours.
- Any per-hour price.
- Hosting a license server for third parties outside the vendor's terms. SideFX's public-interface ban is one example.

---

## 5. Metering building blocks

- **AWS Marketplace Metering Service** (`RegisterUsage` for per-pod hourly, `MeterUsage` for custom units). EKS/ECS only. Source: https://docs.aws.amazon.com/marketplace/latest/userguide/container-products-billing-integration.html
- **AWS License Manager seller-issued licenses:**
  - `CheckoutLicense` takes a product SKU, an issuer key fingerprint, and a PROVISIONAL or PERPETUAL checkout type, and returns a consumption token for check-in.
  - Floating entitlements return to the pool automatically if not checked in, after 60 minutes per the AWS docs (Japanese page, via search index).
  - Works for buyers with or without AWS accounts.
  Sources: https://docs.aws.amazon.com/license-manager/latest/APIReference/API_CheckoutLicense.html ; https://aws.amazon.com/blogs/mt/ensure-license-compliance-aws-isvs-using-isv-seller-issued-licenses/
- **Google ubbagent:** sidecar that aggregates usage and reports to Service Control. Source: https://github.com/GoogleCloudPlatform/ubbagent
- **Azure Marketplace custom meters:** usage events posted to the Marketplace metering API with managed-identity auth. Not available on Arc offers. Sources: https://learn.microsoft.com/en-us/partner-center/marketplace-offers/azure-container-technical-assets-kubernetes ; https://learn.microsoft.com/en-us/azure/marketplace/marketplace-metering-service-authentication
- **OpenMeter:** open-source (Apache 2.0) usage metering and billing. Kong acquired it on 2025-09-03 and is folding it into Konnect, with OpenMeter remaining available as open source and SaaS. Sources: https://konghq.com/blog/news/kong-acquires-openmeter ; https://siliconangle.com/2025/09/03/kong-acquires-openmeter-power-api-ai-monetization/ . A Kubernetes collector component: unverified.
- **Stripe Billing meters:**
  - The v2 meter event stream uses 15-minute session tokens.
  - AWS-style aggregation is recommended (accumulate usage, send periodic events).
  - Throughput: the guide says 10,000 events/s (more via sales) while the changelog says 100,000/s. The figures conflict; unverified.
  Sources: https://docs.stripe.com/billing/subscriptions/usage-based/recording-usage-api ; https://docs.stripe.com/changelog/acacia/2024-09-30/usage-based-billing-v2-meter-events-api
- **Replicated SDK:** in-cluster API (`/api/v1/license/info`, `/api/v1/license/fields`) for vendor-defined entitlements in Helm installs. It is an entitlement source, not a meter. Source: https://docs.replicated.com/vendor/licenses-reference-sdk
- **OpenCost:** CNCF Incubating since October 2024. Allocates CPU, memory, GPU, and PV cost per workload. Useful as the resource-hour source, not as a billing system. Source: https://opencost.io/blog/cncf-incubation
- **Revenera FlexNet Operations metered licensing:** usage capture from FlexNet servers into a producer portal. Source: https://docs.revenera.com/fno_CURRENT/producer/Content/helplibrary/opsGS_UM_MeteredLics.htm
- **Kubernetes-native license operator standard:** none found. No CNCF project or Kubernetes SIG standard for software-license metering surfaced in searches.

---

## Implications for the design

1. **Cite these program names exactly:** AWS ISV Accelerate (via the APN Software Path, needing Validated or Differentiated status); Google Cloud Partner Network (successor to Partner Advantage, per third-party sources); Microsoft AI Cloud Partner Program, Frontier Accelerate for Marketplace, Azure IP co-sell eligible, and MACC; Oracle PartnerNetwork Cloud Build Track; CoreWeave Partner Network. Do not claim "Deployed on AWS" for a Helm product; that badge is defined for SaaS listings.
2. **Ship one Helm chart with per-marketplace metering adapters:**
   - AWS `RegisterUsage`/`MeterUsage` (EKS only).
   - GCP ubbagent sidecar (GKE/GKE Enterprise, x86).
   - Azure custom meters (AKS only; Arc offers get pre-defined per-core/node/pod billing only).
   - OCI: free/BYOL listing only, because Oracle does not allow paid Helm listings.
   - CoreWeave: no marketplace, so direct contract or Stripe/OpenMeter billing.
3. **Make the metering adapter tamper-resistant by construction:** trusted product-code allow-list, metering inside the operator binary rather than an overridable entrypoint, and Region and credentials discovered at runtime. These are AWS's stated rules, and they are the right default everywhere.
4. **Meter c4-operator itself in units each marketplace accepts:** per-pod or per-node hourly works on AWS, Azure, and GCP alike. A custom "render-hour" or "cluster-hour" dimension needs `MeterUsage`, ubbagent, or Azure custom meters, and integer delta reporting on GCP.
5. **Paid listings are a prerequisite for co-sell value.** BYOL and free offers do not count toward Azure IP co-sell or MACC, and ISVA needs Marketplace-transacted opportunities. A free open-source operator alone earns no co-sell standing; a paid, transactable plan is required.
6. **Thresholds to plan around:** Azure IP co-sell needs USD 100k trailing-12-month billed sales. ISVA needs 5 launched and 15 qualified opportunities plus $2,000 recognized revenue. Neither is reachable at launch, so the realistic early step is marketplace listing plus co-sell-ready status.
7. **Licensing broker scope:**
   - Build a broker that schedules against licenses and observes them: per-task admission against concurrency caps, NetworkPolicy and env injection of `port@host`, REPORTLOG parsing for per-task hours.
   - Build a Deadline Cloud license-endpoint adapter for AWS customers. It is the only vendor-sanctioned per-hour pool reachable from a customer's own Kubernetes workers (CMF products: Arnold, Cinema 4D, Nuke, Red Giant, Redshift, Houdini/Mantra/Karma, V-Ray).
   - Note the endpoint lives in an AWS VPC, so it does not serve GKE, AKS, OKE, or CoreWeave workers.
8. **The broker must not promise:**
   - Minting or signing vendor licenses.
   - Short-lived grants that vendor clients honor natively.
   - Per-hour pricing for any vendor.
   - UBL on non-AWS clouds.
   - Maya or 3ds Max UBL on customer-managed workers (Deadline Cloud makes those SMF-only).
   Each of these requires a vendor agreement such as Chaos's RFSA or a Thinkbox-style certificate deal.
9. **Position against Deadline 10's maintenance mode with care.** Deadline 10 still works and is supported for security fixes, with no end-of-life date. The accurate message is "no new features since 2025-11-07, legacy UBL retired 2025-09-30," not "Deadline is dead."
10. **Abstract storage on S3 with two provider quirks:** virtual-hosted addressing always (CoreWeave rejects path-style), and an Azure Blob adapter or gateway, since Blob has no native S3 API. GCS (HMAC plus XML API) and OCI (Customer Secret Keys) work through the S3 path with configuration.
11. **Abstract capacity on the Karpenter NodePool API where it exists:** EKS, EKS Auto Mode, AKS NAP, and OKE KPO. Add ComputeClass for GKE and Cluster Autoscaler node pools for CoreWeave, and budget for CoreWeave's 5 to 15 minute bare-metal scale-up in burst SLAs.
12. **Key management:** use a pluggable KMS interface (AWS KMS, Cloud KMS, Key Vault, OCI Vault) plus a Vault/KMIP backend. CoreWeave has no KMS product, only SSE-C for object storage; its Remote Key Encryption is announced but not documented.
