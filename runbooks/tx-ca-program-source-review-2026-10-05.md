# Texas and California program source review packet — 2026-10-05

Operator research only. None of these leads is a `program_sources` registration, a
terms/access review, a verified offer, or proof that a household qualifies. The
existing `internal/programs/source_review_queue.json` remains disabled. Page
observations below are leads for capture and human review, not import values.
Publication requires a captured immutable source artifact, human access/terms
review, field evidence, verified geographic and product scope, dated terms,
and a current availability check.

## Queue advance on October 5

Four additional official leads were recorded as disabled candidates in
`internal/programs/source_review_queue.json`: Texas TDHCA WAP, California CEC
residential rebates, California CSD weatherization, and LADWP Consumer Rebate
Program. Their candidate dates are October 5; the four earlier queue entries
retain October 3 observation dates. That first pass had eight candidates; it
contains no approved registrations or imported offers. These page observations are not
immutable source captures or permission to fetch them with the Cloud worker.

Six further disabled review leads now cover an IRS historical-credit gate,
Harris County home repair, CPS Energy Casa Verde, SCE Energy Savings Assistance,
Bosch promotions, and the DOE Tribal HEAR administrator gateway. The queue has
14 candidates after this research pass. No candidate is an approved registration,
current household offer, or autonomous fetch target. The DOE gateway is federal
evidence for identifying sovereign administrators, not a Tribal administrator.

| Lead | Official page observation | Field and geography evidence still needed |
| --- | --- | --- |
| [TDHCA WAP](https://www.tdhca.texas.gov/weatherization-assistance-program) | Subrecipients collectively cover 254 counties. | Current county assignment, subrecipient capacity, household income and measure rules; statewide administrative coverage alone does not establish household service. |
| [CEC residential rebates](https://www.energy.ca.gov/programs-and-topics/programs/inflation-reduction-act-residential-energy-rebate-programs) | HEEHRA Phase I continues processing completed projects with approved reservations; the page distinguishes Phase II and HOMES. | Current TECH regional budget, reservation availability, trained contractor, verified income, dwelling and equipment conditions for a particular phase. |
| [CSD residential efficiency](https://www.csd.ca.gov/Pages/Residential-Energy-Efficiency.aspx) | WAP uses local providers; LIWP has separate farmworker and multifamily components. | Provider geography, capacity, dwelling, income and measure rules for each program; these are conditional services rather than fixed rebates. |
| [LADWP Consumer Rebate Program](https://www.ladwp.com/residential-services/assistance-programs/consumer-rebate-program?redirect=false) | Equipment-specific rebates require active residential electric meter service. | Controlling application and product tables, address-level service, equipment thresholds, purchase/install/application windows and current funds. The page may redirect to another official LADWP host, which needs separate host and access review. |

## Broader official-source leads

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
| Federal tax | TX + CA historical | [IRS Form 5695 instructions](https://www.irs.gov/instructions/i5695) confirm termination of residential 25C and 25D credits for relevant dates after 2025. | Historical tax review only; distinguish expenditure and placed-in-service tests and never imply a current 2026 purchase credit. |
| Tribal | TX + CA, nation-specific | [DOE intended Tribal HEAR allocations](https://www.energy.gov/sites/default/files/2023-08/DOE-Intended-Allocations-for-the-Tribal-Electrification-and-Appliance-Rebates-Program-8.16.2023.pdf) names TX and CA Tribal governments; [DOE fact sheet](https://www.energy.gov/sites/default/files/2024-04/tribal-rebates-fact-sheet_040424.pdf) says each Tribe applies for a grant and can tailor its program. | Allocation is not an open household program. Find each sovereign administrator's current program notice and approved service geography before any offer import. |

## Additional administrator coverage and boundaries

These are representative official leads, not a census or a finding that an offer
is open. A program for general housing repair, bill relief, or public buildings
must be screened for product relevance before it enters appliance discovery.

| Administrator category | Official lead | Review boundary |
| --- | --- | --- |
| Federal tax authority | [IRS 2025 Form 5695 instructions](https://www.irs.gov/instructions/i5695) | Historical 25C/25D review; the controlling date and taxpayer facts matter. DOE's [Home Energy Rebates overview](https://www.energy.gov/cmei/scep/home-energy-rebates-program) is a federal gateway, while states and Tribes set local delivery terms. |
| TX state agency | [SECO home rebates](https://comptroller.texas.gov/programs/seco/funding/ira/) and [TDHCA WAP](https://www.tdhca.texas.gov/weatherization-assistance-program) | SECO HOMES/HEAR are not launched; WAP requires a local subrecipient and capacity check. |
| CA state agency | [CEC IRA rebates](https://www.energy.ca.gov/programs-and-topics/programs/inflation-reduction-act-residential-energy-rebate-programs) and [CSD residential efficiency](https://www.csd.ca.gov/Pages/Residential-Energy-Efficiency.aspx) | Split CEC phases and TECH regional funding; split CSD WAP and LIWP local delivery. |
| TX city / municipal utility | [Austin Energy residential programs](https://austinenergy.com/energy-efficiency/rebates-incentives/residential) and [CPS Energy Casa Verde](https://www.cpsenergy.com/casaverde) | Check actual electric service, qualified installer, income, measure and funding rules; municipal name or ZIP does not prove service. |
| CA city / municipal utility | [LADWP Consumer Rebate Program](https://www.ladwp.com/residential-services/assistance-programs/consumer-rebate-program?redirect=false) and [San José Clean Energy EcoHome agreement](https://sanjosecleanenergy.org/wp-content/uploads/2025/10/EcoHome-Rebate-Applicant-Agreement.pdf) | Check meter or retail electricity provider and current enrollment; an agreement alone does not prove available funds. |
| TX county | [Harris County Home Repair Program](https://hcd.harriscountytx.gov/Residents/Home-Repair-Program) | The page says current applications are only for septic and wells, so it yields no current appliance or energy offer. Verify any later intake change and county service boundary. Other TX county administrators remain unenumerated. |
| CA county | [Marin County Electrify Marin](https://www.marincounty.gov/departments/executive/climate-and-sustainability/sustainability/electrify-marin) | Verify active rebate funding, parcel boundary, controlling form and appliance replacement conditions. Other CA county administrators remain unenumerated. |
| TX utility | [Oncor weatherization](https://www.oncor.com/content/oncorwww/talot/en/home/get-started/residential/weatherization.html), [Texas Gas Service efficiency](https://www.texasgasservice.com/save-money/energy-saving-tools-tips/energy-efficiency-program), and [CPS Energy Casa Verde](https://www.cpsenergy.com/casaverde) | Oncor's 2026 funding is exhausted and Texas Gas Service describes an expired program; monitor reopening. Distinguish electric delivery, gas service and municipal utility territories. |
| CA utility | [PG&E Energy Savings Assistance](https://www.pge.com/en/save-energy-and-money/energy-saving-programs/energy-savings-assistance-program.html), [SCE Energy Savings Assistance](https://www.sce.com/save-money/income-qualified-programs/energy-savings-assistance-program), and [SDG&E ESA Whole Home](https://www.sdge.com/residential/pay-bill/get-payment-bill-assistance/assistance-programs/energy-savings-assistance-whole-home-program) | Check active meter, income and dwelling rules, measure availability and current intake. SCE's separate building electrification and ESA Whole Home components report closed enrollment. |
| OEM | [GE Appliances promotions](https://www.geappliances.com/ge/rebates-promotions) and [Bosch specials](https://www.bosch-home.com/us/specials/) | Directories require individual forms, SKU, retailer, geography, purchase and claim windows; Bosch's package promotion is not a single-device award. |
| Sovereign Tribal administrator | [DOE Tribal HEAR fact sheet](https://www.energy.gov/sites/default/files/2024-04/tribal-rebates-fact-sheet_040424.pdf) and [DOE intended allocations](https://www.energy.gov/sites/default/files/2023-08/DOE-Intended-Allocations-for-the-Tribal-Electrification-and-Appliance-Rebates-Program-8.16.2023.pdf) identify a grant route and candidate governments only. | No current TX or CA Tribe-issued household HEAR notice was verified in this pass. Identify the responsible sovereign government or consortium, award and launch status, citizenship/residency rules, territory and funds directly with that administrator. |

Access terms and robots permissions were **not** reviewed for any listed host.
The links are manual research leads. Each distinct host, redirect and PDF needs
operator approval and an immutable capture before automated retrieval, mapping,
registration or offer import. Current funding and application status must be
checked again at review time; the observation date is not a validity window.

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
