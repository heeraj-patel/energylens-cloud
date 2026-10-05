# Texas and California program source review packet — 2026-10-05

Operator research only. None of these leads is a `program_sources` registration, a
terms/access review, a verified offer, or proof that a household qualifies. The
existing `internal/programs/source_review_queue.json` remains disabled. Page
observations below are leads for capture and human review, not import values.
Publication requires a captured immutable source artifact, human access/terms
review, field evidence, verified geographic and product scope, dated terms,
and a current availability check.

| Layer | Geography | Official lead and observed review question | Publication gate |
| --- | --- | --- | --- |
| State | TX | [SECO home energy rebates](https://comptroller.texas.gov/programs/seco/funding/ira/) says HOMES and HEAR rebates are unavailable while Texas designs them (page update August 21, 2026). | Monitor launch and DOE approval; do not publish the advertised maximums as available offers. |
| State / federal delivery | TX | [TDHCA Weatherization Assistance Program](https://www.tdhca.texas.gov/weatherization-assistance-program) describes income-targeted services through subrecipients across 254 counties. | Obtain current subrecipient geography, service capacity, household and measure rules; service is not a fixed cash rebate. |
| State | CA | [CEC IRA residential rebate page](https://www.energy.ca.gov/programs-and-topics/programs/inflation-reduction-act-residential-energy-rebate-programs) describes HEEHRA Phase I and HOMES. CEC says HEEHRA requires income verification, a trained contractor and approved reservation before purchase. | Check live regional budget and reservation status, controlling TECH documents, dwelling/technology scope, and dates. Do not equate CEC's headline maximum with a household award. |
| State / federal delivery | CA | [CSD residential energy efficiency](https://www.csd.ca.gov/Pages/Residential-Energy-Efficiency.aspx) lists WAP and LIWP with local providers and household eligibility review. | Obtain current provider/service map and measure rules; distinguish in-kind services from cash benefits. |
| City / municipal utility | TX, Austin | [Austin Energy residential rebates](https://austinenergy.com/energy-efficiency/rebates-incentives/residential) lists appliance and home improvement programs. | Review each linked offer and Austin Energy service requirement; a ZIP match is only a candidate. Existing instant-savings queue lead remains unreviewed. |
| City / municipal utility | CA, Los Angeles | [LADWP Consumer Rebate Program](https://www.ladwp.com/residential-services/assistance-programs/consumer-rebate-program?redirect=false) lists equipment-specific rebates and requires active residential electric service. | Capture current application and product tables; validate purchase/install/application windows, model thresholds, and address service. |
| City / community choice | CA, San José | [San José Clean Energy EcoHome applicant agreement](https://sanjosecleanenergy.org/wp-content/uploads/2025/10/EcoHome-Rebate-Applicant-Agreement.pdf) contains measure and dwelling limits. | Confirm current enrollment/funding on administrator site and retail electricity service; do not infer offer availability from an agreement alone. |
| County | CA, Marin | [Electrify Marin](https://www.marincounty.gov/departments/executive/climate-and-sustainability/sustainability/electrify-marin) links county appliance replacement rebates; its [application](https://www.marincounty.gov/sites/g/files/fdkgoe241/files/2024-08/marinappliancerebateappaug14.24update.pdf) says first come, first served. | Confirm current funding and controlling form; verify parcel/county boundary and permit, income and fuel-replacement conditions. |
| Utility | TX, Oncor | [2026 low-income weatherization page](https://www.oncor.com/content/oncorwww/talot/en/home/get-started/residential/weatherization.html) says 2026 funding is exhausted. | Check program-year reopening and distributor service; never publish as an open 2026 benefit. |
| Utility | CA, PG&E | [Energy Savings Assistance](https://www.pge.com/en/save-energy-and-money/energy-saving-programs/energy-savings-assistance-program.html) describes no-cost measures with income, dwelling and assessment conditions. | Review current service territory and household screening; benefit is conditional service, not a fixed cash amount. |
| OEM | TX + CA candidate | [GE Appliances rebates and promotions](https://www.geappliances.com/ge/rebates-promotions) lists time-limited model/retailer promotions. | Capture individual controlling rebate forms, eligible models, locations, purchase and claim windows; directory is not an offer. Existing GE queue lead remains unreviewed. |
| OEM | TX + CA candidate | [Bosch specials](https://www.bosch-home.com/us/specials/) lists a 2026 package rebate; its [California wildfire relief offer](https://www.bosch-home.com/us/specials/wildfire-relief-rebate/) specifies eight California counties and an insurance claim. | Review forms, model bundles, retailer and claim rules; do not map a one-device inventory item to a multi-appliance package automatically. |
| Federal tax | TX + CA historical | [IRS 2025-law change FAQ](https://www.irs.gov/newsroom/faqs-for-modification-of-sections-25c-25d-25e-30c-30d-45l-45w-and-179d-under-public-law-119-21-139-stat-72-july-4-2025-commonly-known-as-the-one-big-beautiful-bill-obbb) says 25C is unavailable for property placed in service after December 31, 2025; 25D is unavailable for expenditures after that date. | Keep out of 2026 purchase recommendations. Historical tax treatment needs the qualifying date and taxpayer review. Older IRS pages still show superseded end dates. |
| Tribal | TX + CA, nation-specific | [DOE intended Tribal HEAR allocations](https://www.energy.gov/sites/default/files/2023-08/DOE-Intended-Allocations-for-the-Tribal-Electrification-and-Appliance-Rebates-Program-8.16.2023.pdf) names TX and CA Tribal governments; [DOE fact sheet](https://www.energy.gov/sites/default/files/2024-04/tribal-rebates-fact-sheet_040424.pdf) says each Tribe applies for a grant and can tailor its program. | Allocation is not an open household program. Find each sovereign administrator's current program notice and approved service geography before any offer import. |

## Work required before coverage can be called comprehensive

1. Human operator reviews site access terms/robots, host allowlists and fetch
   budgets for every source; capture the controlling page/PDF bytes and review
   dates. This packet itself supplies no access approval.
2. Enumerate Texas and California state, municipal, county, Tribal, utility and
   OEM administrators. The examples above establish leads, not a complete
   census. Texas county-administered and individual Tribal household offers
   remain especially unverified.
3. Resolve official service boundaries and ZIP-to-provider candidates without
   treating ZIP presence as address-level service. Record negative, unavailable,
   waitlisted and unknown outcomes as such; a checked scope does not imply an
   available program.
4. For each live offer, record exact source-backed amount type (fixed amount,
   percentage, cap, per-unit, in-kind service or variable), eligible model and
   technology, income, address, provider, contractor, purchase, installation,
   reservation, application and stacking conditions. Keep unsatisfied household
   facts unknown, not ineligible or approved.
5. Import only after independent human review; then verify the owner API and
   mobile journey against real reviewed terms. Customer-facing copy needs owner
   approval before mobile work.
