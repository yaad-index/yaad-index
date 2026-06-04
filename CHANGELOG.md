# Changelog

## [0.18.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.17.0...yaad-index-v0.18.0) (2026-06-04)


### Features

* **api,store,vault,mcp:** rename user-content ([#425](https://github.com/yaad-index/yaad-index/issues/425) Cut 2) ([791aedd](https://github.com/yaad-index/yaad-index/commit/791aedd1011e423d8ae40814484dd58951bcc0e0))
* **api,store:** filter needs_fill by source and kind — yaad-index half ([#385](https://github.com/yaad-index/yaad-index/issues/385)) ([#411](https://github.com/yaad-index/yaad-index/issues/411)) ([78d26fe](https://github.com/yaad-index/yaad-index/commit/78d26fe8f501d17d4ad0b9eb53ec956f510fbf7a))
* **api,vault,mcp:** in-place move for user-content ([#425](https://github.com/yaad-index/yaad-index/issues/425) Cut 1) ([83b3e16](https://github.com/yaad-index/yaad-index/commit/83b3e163f65fc5dd7b1914263813e940e06f9e9d))
* **api,vault,mcp:** optional subfolder for user-content create ([#415](https://github.com/yaad-index/yaad-index/issues/415)) ([e7632a4](https://github.com/yaad-index/yaad-index/commit/e7632a4996be53fe1921514c8d140de249b93a21))
* **api,vault:** route task-kind notes through the 5-section AddNote primitive ([#343](https://github.com/yaad-index/yaad-index/issues/343)) ([eb22ffe](https://github.com/yaad-index/yaad-index/commit/eb22ffe43b013a8cfe55df19d3ae4d26bfa70a17))
* **auth,api:** agent-on-behalf-of-operator fill via OperatorDelegated claim ([#361](https://github.com/yaad-index/yaad-index/issues/361)) ([#419](https://github.com/yaad-index/yaad-index/issues/419)) ([c63c2ad](https://github.com/yaad-index/yaad-index/commit/c63c2ad10a6b1faa5363a99679ee733fda64fd5d))
* **gmail:** surface forwarded original sender + subject ([#323](https://github.com/yaad-index/yaad-index/issues/323)) ([eaa24b0](https://github.com/yaad-index/yaad-index/commit/eaa24b0aeda6bca87552acb211a3b16d948a0748))
* **mcp:** batch archive/delete/task-resolve tools ([#383](https://github.com/yaad-index/yaad-index/issues/383)) ([2e18f83](https://github.com/yaad-index/yaad-index/commit/2e18f831b099d6579bc005bf06a61fa44bed03bb))


### Bug Fixes

* **api:** restore fill provenance stamp + top-level summary/tags routing ([#358](https://github.com/yaad-index/yaad-index/issues/358), [#359](https://github.com/yaad-index/yaad-index/issues/359)) ([1611194](https://github.com/yaad-index/yaad-index/commit/1611194e54ad937f46794efcef487a9926f791eb))
* **store:** tokenize-and-AND search so multi-word queries span punctuation ([#391](https://github.com/yaad-index/yaad-index/issues/391)) ([#410](https://github.com/yaad-index/yaad-index/issues/410)) ([eacf847](https://github.com/yaad-index/yaad-index/commit/eacf847ccb5759cb4644fe45e6872af0d1c3e4f4))

## [0.17.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.16.0...yaad-index-v0.17.0) (2026-06-01)


### Features

* **api,canonical,workflow:** auto-alias entity to source-of-slug name ([#405](https://github.com/yaad-index/yaad-index/issues/405)) ([#406](https://github.com/yaad-index/yaad-index/issues/406)) ([2be3972](https://github.com/yaad-index/yaad-index/commit/2be3972ebe12479706d297f5b8de7b79b01bf00a))

## [0.16.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.15.0...yaad-index-v0.16.0) (2026-06-01)


### Features

* **api,mcp:** create_canonical_entity — direct canonical-entity creation ([#389](https://github.com/yaad-index/yaad-index/issues/389)) ([#398](https://github.com/yaad-index/yaad-index/issues/398)) ([2c67f50](https://github.com/yaad-index/yaad-index/commit/2c67f5004dc7578538197422db5595b829bfd8f9))
* **api,mcp:** edit_note + delete_note ([#390](https://github.com/yaad-index/yaad-index/issues/390) Cut 2) ([#401](https://github.com/yaad-index/yaad-index/issues/401)) ([97143e7](https://github.com/yaad-index/yaad-index/commit/97143e7a42ed9ec07f95340bcc77a255ebf1f19f))
* **api,store:** alias-as-resolver — id tools resolve aliases ([#392](https://github.com/yaad-index/yaad-index/issues/392)) ([#402](https://github.com/yaad-index/yaad-index/issues/402)) ([b24796f](https://github.com/yaad-index/yaad-index/commit/b24796f45d0b0f56fa927e850ab45ab7e6a5962a))
* **api:** auto-materialize canonical thin-edges on first body write ([#388](https://github.com/yaad-index/yaad-index/issues/388) Cut 2b) ([#396](https://github.com/yaad-index/yaad-index/issues/396)) ([fa57bb5](https://github.com/yaad-index/yaad-index/commit/fa57bb571b82f505749f4d5314f09a1de1cbda5e))
* **api:** canonical entities accept operator-authored body sections ([#388](https://github.com/yaad-index/yaad-index/issues/388) Cut 2a) ([#394](https://github.com/yaad-index/yaad-index/issues/394)) ([e43ea85](https://github.com/yaad-index/yaad-index/commit/e43ea85927bb497b170d70ea69f823b4d52611ed))
* **vault,api,workflow:** note identity — stable note_id per note ([#390](https://github.com/yaad-index/yaad-index/issues/390) Cut 1) ([#400](https://github.com/yaad-index/yaad-index/issues/400)) ([b0abbb6](https://github.com/yaad-index/yaad-index/commit/b0abbb645c309b7bbc27789c7da389f2e2e30240))

## [0.15.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.14.0...yaad-index-v0.15.0) (2026-05-31)


### Features

* **auth:** operator-keyed UGC edit permission + disambiguate add_note ([#377](https://github.com/yaad-index/yaad-index/issues/377)) ([#378](https://github.com/yaad-index/yaad-index/issues/378)) ([af7753a](https://github.com/yaad-index/yaad-index/commit/af7753a513b6df6a7ce09767b1fdf3ef8a9dd180))
* **workflow:** archive_when engine hook + e2e ([#376](https://github.com/yaad-index/yaad-index/issues/376) Cut 3) ([#382](https://github.com/yaad-index/yaad-index/issues/382)) ([f1158f0](https://github.com/yaad-index/yaad-index/commit/f1158f0c82c6245569c39510c15a1efb69636616))
* **workflow:** archive_when parser + predicate evaluator ([#376](https://github.com/yaad-index/yaad-index/issues/376) Cut 2) ([#381](https://github.com/yaad-index/yaad-index/issues/381)) ([4cb78d3](https://github.com/yaad-index/yaad-index/commit/4cb78d34d848bb93ea9d647329b31c86c9e537a3))

## [0.14.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.13.0...yaad-index-v0.14.0) (2026-05-31)


### Features

* **api:** operator-fill on non-canonical-kind entities ([#353](https://github.com/yaad-index/yaad-index/issues/353)) ([#354](https://github.com/yaad-index/yaad-index/issues/354)) ([0a2fe5b](https://github.com/yaad-index/yaad-index/commit/0a2fe5b5c495345b29119cfc10e1284bd7723f87))
* **api:** unified /v1/fill endpoint + 410 gone ([#355](https://github.com/yaad-index/yaad-index/issues/355) Cut 2a) ([#357](https://github.com/yaad-index/yaad-index/issues/357)) ([20f843d](https://github.com/yaad-index/yaad-index/commit/20f843d333a9daf89c2b582e5a54724c47cf5013))
* **mcp:** expose force_refetch on ingest tool ([#372](https://github.com/yaad-index/yaad-index/issues/372)) ([#373](https://github.com/yaad-index/yaad-index/issues/373)) ([8d28641](https://github.com/yaad-index/yaad-index/commit/8d286419beb0086838d044bd5648ceef4467e5cb))


### Bug Fixes

* **api:** gap-state-aware total on needs_fill ([#350](https://github.com/yaad-index/yaad-index/issues/350)) ([#351](https://github.com/yaad-index/yaad-index/issues/351)) ([aa07a19](https://github.com/yaad-index/yaad-index/commit/aa07a1915efb128ebe4b466476393e9c7b34f1b0))
* **bgg:** strip bgg- prefix from canonical-id search queries ([#363](https://github.com/yaad-index/yaad-index/issues/363)) ([#364](https://github.com/yaad-index/yaad-index/issues/364)) ([2e2fd62](https://github.com/yaad-index/yaad-index/commit/2e2fd6211507f68febb865440d16603600bad562))
* **tasks:** defensive unlink + atomic rollback on auto-archive ([#368](https://github.com/yaad-index/yaad-index/issues/368)) ([#371](https://github.com/yaad-index/yaad-index/issues/371)) ([9ab4692](https://github.com/yaad-index/yaad-index/commit/9ab469240ef5f3666177259e5d70b0e9ecb7d69f))

## [0.13.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.12.0...yaad-index-v0.13.0) (2026-05-29)


### Features

* **actions:** 5-section task body schema ([#337](https://github.com/yaad-index/yaad-index/issues/337) Cut 1) ([#339](https://github.com/yaad-index/yaad-index/issues/339)) ([d2ab386](https://github.com/yaad-index/yaad-index/commit/d2ab38639e0921c8c5c96f71ce419003a3a18e00))
* **actions:** bounded task-body primitives ([#337](https://github.com/yaad-index/yaad-index/issues/337) Cut 2) ([#342](https://github.com/yaad-index/yaad-index/issues/342)) ([a2aedbe](https://github.com/yaad-index/yaad-index/commit/a2aedbea12cf718ab79f0eef5d4d091309a0cf4e))
* **actions:** err-task prompt section via SetPrompt ([#344](https://github.com/yaad-index/yaad-index/issues/344)) ([#346](https://github.com/yaad-index/yaad-index/issues/346)) ([6c55098](https://github.com/yaad-index/yaad-index/commit/6c5509833494607ea95ffaff71b2495744dddfad))
* **actions:** resolution-task prompt + edges via SetPrompt ([#345](https://github.com/yaad-index/yaad-index/issues/345)) ([#347](https://github.com/yaad-index/yaad-index/issues/347)) ([50e9642](https://github.com/yaad-index/yaad-index/commit/50e964255721de02c1dbcb1200cfe97c3391cfa7))
* **api,mcp,vault:** UGC section-level CRUD — add + rename + delete ([#299](https://github.com/yaad-index/yaad-index/issues/299)) ([#300](https://github.com/yaad-index/yaad-index/issues/300)) ([be216fb](https://github.com/yaad-index/yaad-index/commit/be216fb51afa5eaecef2f0681914d856028d367e))
* **api,mcp:** resolution-task resolve flow → ingest + edge + archive ([#304](https://github.com/yaad-index/yaad-index/issues/304) Cut C3.3) ([#311](https://github.com/yaad-index/yaad-index/issues/311)) ([0da1561](https://github.com/yaad-index/yaad-index/commit/0da15619d5bf9b18ae65824671e4012bbf0341f1))
* **api:** add total field to paginated responses ([#338](https://github.com/yaad-index/yaad-index/issues/338)) ([#349](https://github.com/yaad-index/yaad-index/issues/349)) ([0c95fb8](https://github.com/yaad-index/yaad-index/commit/0c95fb8322464f0c653d148bdc96071467d4d232))
* **edgewrite,workflow,api:** caller-mode + auto-mode plugin resolution ([#304](https://github.com/yaad-index/yaad-index/issues/304) Cut C2) ([#308](https://github.com/yaad-index/yaad-index/issues/308)) ([f68a45e](https://github.com/yaad-index/yaad-index/commit/f68a45e108be2d0b801d44b90434844971009235))
* **edgewrite:** centralized edge-write service + cardinality enforcement ([#304](https://github.com/yaad-index/yaad-index/issues/304) Cut C1) ([#307](https://github.com/yaad-index/yaad-index/issues/307)) ([0db9d40](https://github.com/yaad-index/yaad-index/commit/0db9d40d2693e36b12c512a5cefec8102d1d7a7a))
* **edgewrite:** resolver_plugin auto-fetch on canonical-edge writes ([#325](https://github.com/yaad-index/yaad-index/issues/325)) ([#327](https://github.com/yaad-index/yaad-index/issues/327)) ([99cf65e](https://github.com/yaad-index/yaad-index/commit/99cf65e2f6ebb5d44ffa20d74581a6da11a648b3))
* **plugins,api:** plugin capability resolves_canonical_kinds + ownership map ([#304](https://github.com/yaad-index/yaad-index/issues/304) Cut A) ([#305](https://github.com/yaad-index/yaad-index/issues/305)) ([85c0a71](https://github.com/yaad-index/yaad-index/commit/85c0a71c45c4ad0d3bdc4733cab60ad2d38946af))
* **store,api,mcp:** update_edge_target primitive — transactional edge rewrite ([#304](https://github.com/yaad-index/yaad-index/issues/304) Cut B) ([#306](https://github.com/yaad-index/yaad-index/issues/306)) ([e5160ac](https://github.com/yaad-index/yaad-index/commit/e5160ac873b92281b7310a39a8cf50cffed485bd))
* **workflow,engine:** catch ResolutionDeferred → spawn resolution-task ([#304](https://github.com/yaad-index/yaad-index/issues/304) Cut C3.2) ([#310](https://github.com/yaad-index/yaad-index/issues/310)) ([ad50e1d](https://github.com/yaad-index/yaad-index/commit/ad50e1d85a0c3f8cfaaeafb8137b0be50f1d0559))
* **workflow:** structured resolution-task primitive ([#304](https://github.com/yaad-index/yaad-index/issues/304) Cut C3.1) ([#309](https://github.com/yaad-index/yaad-index/issues/309)) ([f80b56f](https://github.com/yaad-index/yaad-index/commit/f80b56f6dc31c572f58d9973fef691dd869ff0f3))
* **yaad-bgg:** support boardgameexpansion thing type ([#334](https://github.com/yaad-index/yaad-index/issues/334) Cut 1) ([#336](https://github.com/yaad-index/yaad-index/issues/336)) ([7c767c9](https://github.com/yaad-index/yaad-index/commit/7c767c93aaed0e6a76878ace87f6b30f443ec0d4))


### Bug Fixes

* **api:** drop operator-authority gate on operator-fill endpoint ([#317](https://github.com/yaad-index/yaad-index/issues/317)) ([#318](https://github.com/yaad-index/yaad-index/issues/318)) ([cccdf97](https://github.com/yaad-index/yaad-index/commit/cccdf97b1640aba4822ae6ec3aa13b2242cb5a9f))
* **edgewrite,api:** shared resolver auto-fetch path for fill-gate + edge-write hook ([#325](https://github.com/yaad-index/yaad-index/issues/325)) ([#328](https://github.com/yaad-index/yaad-index/issues/328)) ([4b6da6c](https://github.com/yaad-index/yaad-index/commit/4b6da6cc0768cc0b73ce45e6449190bfe70362f9))
* **edgewrite:** move recursion break from shared dispatch method to CreateEdge call sites ([#330](https://github.com/yaad-index/yaad-index/issues/330)) ([#331](https://github.com/yaad-index/yaad-index/issues/331)) ([d2bcb75](https://github.com/yaad-index/yaad-index/commit/d2bcb7582fd6a56430a7e640f08a7d31b8b45318))
* **vault,workflow:** auto-commit entity subtree + task-body updates ([#314](https://github.com/yaad-index/yaad-index/issues/314)) ([#315](https://github.com/yaad-index/yaad-index/issues/315)) ([e8a2601](https://github.com/yaad-index/yaad-index/commit/e8a2601b1169418d098bbfd040b750dda49ee99c))
* **workflow:** clear gap_call_done_at on add_gap ([#324](https://github.com/yaad-index/yaad-index/issues/324)) ([#326](https://github.com/yaad-index/yaad-index/issues/326)) ([0f4685e](https://github.com/yaad-index/yaad-index/commit/0f4685ef7d6980384cc62282c43a9ebc708fdc08))
* **yaad-bgg:** prefer single exact-name match in disambiguation ([#329](https://github.com/yaad-index/yaad-index/issues/329)) ([#335](https://github.com/yaad-index/yaad-index/issues/335)) ([e6790c7](https://github.com/yaad-index/yaad-index/commit/e6790c78f3d5e1817c13c506f707e2805db62e39))

## [0.12.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.11.0...yaad-index-v0.12.0) (2026-05-26)


### Features

* **api,mcp:** canonical_registry effective + available routes ([#48](https://github.com/yaad-index/yaad-index/issues/48) slice 3) ([#296](https://github.com/yaad-index/yaad-index/issues/296)) ([574a748](https://github.com/yaad-index/yaad-index/commit/574a748ad7c81632cf5b8c208cdebec341daf8ff))
* **api,mcp:** workflow per-name CRUD surface ([#277](https://github.com/yaad-index/yaad-index/issues/277)) ([#292](https://github.com/yaad-index/yaad-index/issues/292)) ([430e19a](https://github.com/yaad-index/yaad-index/commit/430e19af7ba5ccbda32b92da2cc228081a64f9ef))
* **config,api:** canonical_type fill resolver_plugin gate ([#278](https://github.com/yaad-index/yaad-index/issues/278)) ([72b2ba5](https://github.com/yaad-index/yaad-index/commit/72b2ba5c2e84ce624b56f4fe4939374d60afef45))
* **config:** boot-time canonical-registry audit log ([#48](https://github.com/yaad-index/yaad-index/issues/48) slice 4) ([#297](https://github.com/yaad-index/yaad-index/issues/297)) ([17ae6b7](https://github.com/yaad-index/yaad-index/commit/17ae6b79cad2f1415ba5efc5f4363993504215e2))
* **config:** expand ${NAME} env references in plugin instance env values ([#256](https://github.com/yaad-index/yaad-index/issues/256)) ([#283](https://github.com/yaad-index/yaad-index/issues/283)) ([1be1d54](https://github.com/yaad-index/yaad-index/commit/1be1d541d4f5fe665b4d14d1966acfe942f7c3fe))
* **config:** Layer 1.5 daemon-shipped gap defaults for 5 common kinds ([#48](https://github.com/yaad-index/yaad-index/issues/48) slice 2) ([#295](https://github.com/yaad-index/yaad-index/issues/295)) ([a811e8e](https://github.com/yaad-index/yaad-index/commit/a811e8e7de8b8e9aab5c17c16051034c1b5bcb27))
* **gmail,canonical:** surface from/to/cc/bcc on gmail data + daemon-manage gmail canonical vocabulary ([#273](https://github.com/yaad-index/yaad-index/issues/273)) ([95ef2b0](https://github.com/yaad-index/yaad-index/commit/95ef2b0b335b32e836fe96837ded1130f603d57e))
* **needs-fill:** de-dup canonical_vocabulary to response root + ?exclude= opt-out ([#279](https://github.com/yaad-index/yaad-index/issues/279)) ([75d1de6](https://github.com/yaad-index/yaad-index/commit/75d1de637b93eb49c51a81c9e80fffe0e52778ca))
* **plugins:** YAAD_PLUGIN_DATA_DIR per-instance persistent state ([#284](https://github.com/yaad-index/yaad-index/issues/284)) ([#285](https://github.com/yaad-index/yaad-index/issues/285)) ([ee47226](https://github.com/yaad-index/yaad-index/commit/ee47226e66e4f3a55546ff66d9db655e9e5fbb49))
* **store,api,reindex:** DB-side aliases — index + search JOIN + reindex re-derive ([#3](https://github.com/yaad-index/yaad-index/issues/3)) ([#298](https://github.com/yaad-index/yaad-index/issues/298)) ([bf0061f](https://github.com/yaad-index/yaad-index/commit/bf0061fda0015f2ccb9ea7f7998e4073161ec8d4))
* **store:** WARN-once at IncDroppedCanonicalKind/Edge first hit ([#48](https://github.com/yaad-index/yaad-index/issues/48) slice 1) ([#294](https://github.com/yaad-index/yaad-index/issues/294)) ([9812a30](https://github.com/yaad-index/yaad-index/commit/9812a306a41c9701eca24337a8e2ce86670739dc))
* **tasks,day:** promote tasks to first-class entities + lazy-materialize days on edge-write ([#271](https://github.com/yaad-index/yaad-index/issues/271)) ([122d13e](https://github.com/yaad-index/yaad-index/commit/122d13ea66dc03e7262e38c06decc8cd0246e9a3))
* **workflow:** cross-workflow task_resolve action ([#266](https://github.com/yaad-index/yaad-index/issues/266)) ([#293](https://github.com/yaad-index/yaad-index/issues/293)) ([5f96c0f](https://github.com/yaad-index/yaad-index/commit/5f96c0f72a1f7c26a5bff1143d3ef3ac1a00cb04))
* **workflow:** expose trigger context to CEL env (source, event, timestamp, cause) ([#265](https://github.com/yaad-index/yaad-index/issues/265)) ([b0f4a34](https://github.com/yaad-index/yaad-index/commit/b0f4a341e57832b2a91095cc43f1efd430d39c80))
* **yaad-bgg:** per-game collection enrichment via authenticated session ([#282](https://github.com/yaad-index/yaad-index/issues/282)) ([#288](https://github.com/yaad-index/yaad-index/issues/288)) ([ed22ba4](https://github.com/yaad-index/yaad-index/commit/ed22ba49aea7382e817f9d78674fb739b728965f))


### Bug Fixes

* **config:** extend env-key reservation to STAGING_DIR + TIMEZONE ([#286](https://github.com/yaad-index/yaad-index/issues/286)) ([#290](https://github.com/yaad-index/yaad-index/issues/290)) ([659cad1](https://github.com/yaad-index/yaad-index/commit/659cad1b8c4a5219a5689645e4d415b1c8d92ed0))
* **github:** emit is_about edge to materialize github-pr / github-issue canonical entities ([#261](https://github.com/yaad-index/yaad-index/issues/261)) ([42d93aa](https://github.com/yaad-index/yaad-index/commit/42d93aa924237ea6f38bb4ac0839112b411b023e))
* **plugins:** resolve data dir via plugin_data_root + STATE_DIRECTORY chain ([#287](https://github.com/yaad-index/yaad-index/issues/287)) ([#291](https://github.com/yaad-index/yaad-index/issues/291)) ([280d5da](https://github.com/yaad-index/yaad-index/commit/280d5da92a6b8875f623dc73d8ae701e7022a37a))
* **workflow:** derive entity.slug in CEL env ([#269](https://github.com/yaad-index/yaad-index/issues/269)) ([cf7da16](https://github.com/yaad-index/yaad-index/commit/cf7da16dc544f85c2dd5bbeff1135bffc532113f))
* **workflow:** skip Reconcile no-op via ContentHash to eliminate 15s log flap ([#281](https://github.com/yaad-index/yaad-index/issues/281)) ([85f17e5](https://github.com/yaad-index/yaad-index/commit/85f17e5d84dba297d85361f7a4c06dde254402bc))

## [0.11.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.10.0...yaad-index-v0.11.0) (2026-05-25)


### Features

* ADR-0028 Cut 5 — enabled flag + docs (closes [#241](https://github.com/yaad-index/yaad-index/issues/241) cascade) ([#255](https://github.com/yaad-index/yaad-index/issues/255)) ([806decc](https://github.com/yaad-index/yaad-index/commit/806decc49c4687f481f246bdb149bbedcbf21580))
* **api,github:** ADR-0028 Cut 3 — URL dispatch via instance_routing ([#251](https://github.com/yaad-index/yaad-index/issues/251)) ([39b8cca](https://github.com/yaad-index/yaad-index/commit/39b8cca2d778589fc13a31a93ff59dce67ab28f0))
* **config:** ADR-0028 Cut 1 — instances[] schema + supports_instances gate ([#248](https://github.com/yaad-index/yaad-index/issues/248)) ([19dc5fb](https://github.com/yaad-index/yaad-index/commit/19dc5fb759cf7b8daffa161f7513dc49e9c41fce))
* **plugins,api:** ADR-0028 Cut 4 — command dispatch grammar + serial fan-out ([#253](https://github.com/yaad-index/yaad-index/issues/253)) ([0902107](https://github.com/yaad-index/yaad-index/commit/0902107207b046cc33fc243719f3f6085aa1598a))
* **vault,api:** ADR-0028 Cut 2 — entity source: slash-form everywhere ([#250](https://github.com/yaad-index/yaad-index/issues/250)) ([9cd41ad](https://github.com/yaad-index/yaad-index/commit/9cd41ad200d6f89fd26fc40679d328afa0372f88))

## [0.10.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.9.0...yaad-index-v0.10.0) (2026-05-24)


### Features

* **workflow:** CEL date arithmetic + period helpers ([#231](https://github.com/yaad-index/yaad-index/issues/231), ADR-0027 cut 2) ([#236](https://github.com/yaad-index/yaad-index/issues/236)) ([6d7f10f](https://github.com/yaad-index/yaad-index/commit/6d7f10f634e29cd64620709f4190acb157456942))
* **workflow:** CEL date helpers + action runner kind-prefix strip ([#230](https://github.com/yaad-index/yaad-index/issues/230), ADR-0027 cut 1) ([#234](https://github.com/yaad-index/yaad-index/issues/234)) ([ea95ee5](https://github.com/yaad-index/yaad-index/commit/ea95ee5db4b5210f1a810976e708b67d7fb275fa))
* **workflow:** CEL graph-walk primitives + ext.Lists wiring ([#232](https://github.com/yaad-index/yaad-index/issues/232), ADR-0027 cut 3) ([#237](https://github.com/yaad-index/yaad-index/issues/237)) ([595dca3](https://github.com/yaad-index/yaad-index/commit/595dca3c27228656159ee4eb75295318690dae8d))

## [0.9.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.8.0...yaad-index-v0.9.0) (2026-05-23)


### Features

* **auth:** per-command operator-only flag on plugin CommandSpec ([#107](https://github.com/yaad-index/yaad-index/issues/107)) ([#217](https://github.com/yaad-index/yaad-index/issues/217)) ([c036b12](https://github.com/yaad-index/yaad-index/commit/c036b128de5f3cc9b74e4487d1c0afbe644da4fc))
* **canonical:** day kind + canonical edge vocab + DayLocation ([#220](https://github.com/yaad-index/yaad-index/issues/220), ADR-0025 cut 1) ([#224](https://github.com/yaad-index/yaad-index/issues/224)) ([b2bd6f8](https://github.com/yaad-index/yaad-index/commit/b2bd6f8491a02550292fbab676ce3de4f6531793))
* **canonical:** day-reference shape-scan on write paths ([#221](https://github.com/yaad-index/yaad-index/issues/221), ADR-0025 cut 2) ([#226](https://github.com/yaad-index/yaad-index/issues/226)) ([1602c63](https://github.com/yaad-index/yaad-index/commit/1602c636f076dbc72820b08646ce253b3206a7a3))
* **notes:** accept Field + Kind on write surfaces ([#186](https://github.com/yaad-index/yaad-index/issues/186) Cut 2) ([#215](https://github.com/yaad-index/yaad-index/issues/215)) ([2d979b3](https://github.com/yaad-index/yaad-index/commit/2d979b3d323f9b01c1df123a8da424e255286aa9))
* **notes:** add Field + Kind to the notes data model ([#186](https://github.com/yaad-index/yaad-index/issues/186) Cut 1) ([#213](https://github.com/yaad-index/yaad-index/issues/213)) ([80c059a](https://github.com/yaad-index/yaad-index/commit/80c059a7f291d808dee26d43ea19aae160f69e55))
* **notes:** notes_kind filter on read paths ([#186](https://github.com/yaad-index/yaad-index/issues/186) Cut 3) ([#216](https://github.com/yaad-index/yaad-index/issues/216)) ([4e15c27](https://github.com/yaad-index/yaad-index/commit/4e15c2751e61c7b748c498f125b0ef2fb6e7c492))
* **search:** is_journal filter on /v1/search + MCP list_entities ([#222](https://github.com/yaad-index/yaad-index/issues/222), ADR-0025 cut 3) ([#227](https://github.com/yaad-index/yaad-index/issues/227)) ([525bbd5](https://github.com/yaad-index/yaad-index/commit/525bbd522e5128fb698b358cc3d3e94f0c0f61f1))

## [0.8.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.7.0...yaad-index-v0.8.0) (2026-05-22)


### Features

* **api,mcp:** Streamable HTTP MCP route + bridge + get_entity sample tool ([#101](https://github.com/yaad-index/yaad-index/issues/101) Cut 1) ([#172](https://github.com/yaad-index/yaad-index/issues/172)) ([8440ff4](https://github.com/yaad-index/yaad-index/commit/8440ff4f86fe5240c24000064a0ef009c1874948))
* **config:** structured per-plugin config + JSON Schema validation ([#192](https://github.com/yaad-index/yaad-index/issues/192) foundation) ([#205](https://github.com/yaad-index/yaad-index/issues/205)) ([b0418cb](https://github.com/yaad-index/yaad-index/commit/b0418cbe7e9f631aaf28bbef93b431a039d4e962))
* **mcp:** port remaining 32 daemon tools via the Cut 1 bridge ([#101](https://github.com/yaad-index/yaad-index/issues/101) Cut 2) ([#174](https://github.com/yaad-index/yaad-index/issues/174)) ([d5e8b29](https://github.com/yaad-index/yaad-index/commit/d5e8b299e8c36146d82b1f51cdf8b0ecdc85e2bc))
* **workflow,engine:** sequential FIFO queue + two-pass eval + explicit-claim + catch-all ([#169](https://github.com/yaad-index/yaad-index/issues/169)) ([#170](https://github.com/yaad-index/yaad-index/issues/170)) ([69c27a6](https://github.com/yaad-index/yaad-index/commit/69c27a635239343165f5076b8d6df4dab9639a45))
* **workflow:** entity.updated + field_changed + restore_entity ([#196](https://github.com/yaad-index/yaad-index/issues/196) PR-C) ([#199](https://github.com/yaad-index/yaad-index/issues/199)) ([fdf1029](https://github.com/yaad-index/yaad-index/commit/fdf10293d896ab0131604c9857b35b2bba5bd743))
* **yaad-github:** bulk fetch via Search API ([#187](https://github.com/yaad-index/yaad-index/issues/187) Cut 3) ([#193](https://github.com/yaad-index/yaad-index/issues/193)) ([a7e72fe](https://github.com/yaad-index/yaad-index/commit/a7e72feaa30580ef1bf7738ef0924ecfa3ebcdab))
* **yaad-github:** closed-recent sweep + YAAD_GITHUB_RECENT_DAYS ([#187](https://github.com/yaad-index/yaad-index/issues/187) Cut 4 / [#196](https://github.com/yaad-index/yaad-index/issues/196) PR-D) ([#200](https://github.com/yaad-index/yaad-index/issues/200)) ([2c415ca](https://github.com/yaad-index/yaad-index/commit/2c415cafbdcc9bd52dc5e61a852f3bd73b515b3d))
* **yaad-github:** migrate to YAAD_PLUGIN_CONFIG + config_schema ([#192](https://github.com/yaad-index/yaad-index/issues/192)) ([#209](https://github.com/yaad-index/yaad-index/issues/209)) ([28fcb76](https://github.com/yaad-index/yaad-index/commit/28fcb7613d805ccae88f2846ee7924cc74c578df))
* **yaad-github:** scaffold + --version + --init + auth wiring ([#187](https://github.com/yaad-index/yaad-index/issues/187) Cut 1) ([#188](https://github.com/yaad-index/yaad-index/issues/188)) ([94bf0a7](https://github.com/yaad-index/yaad-index/commit/94bf0a7ce332ebd745a75752395ff710763f5715))
* **yaad-github:** URL-shape single-item PR/issue fetch ([#187](https://github.com/yaad-index/yaad-index/issues/187) Cut 2) ([#189](https://github.com/yaad-index/yaad-index/issues/189)) ([c0238c8](https://github.com/yaad-index/yaad-index/commit/c0238c8f6f87fbbdbcc2190bf32032fa53f06e33))

## [0.7.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.6.0...yaad-index-v0.7.0) (2026-05-18)


### Features

* **workflow,actions:** extend wikilink emission to add_note + set_property ([#166](https://github.com/yaad-index/yaad-index/issues/166)) ([#167](https://github.com/yaad-index/yaad-index/issues/167)) ([25fd5fd](https://github.com/yaad-index/yaad-index/commit/25fd5fdd4f4912fde626c1b8b95f9a58c3e30467))

## [0.6.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.5.0...yaad-index-v0.6.0) (2026-05-18)


### Features

* **workflow,actions:** always-on Via breadcrumbs + wikilink emission ([#163](https://github.com/yaad-index/yaad-index/issues/163)) ([#165](https://github.com/yaad-index/yaad-index/issues/165)) ([99fa6b8](https://github.com/yaad-index/yaad-index/commit/99fa6b868fca52b638fca8ce0832bcae82d9dfef))
* **workflow,actions:** archive_entity action ([#150](https://github.com/yaad-index/yaad-index/issues/150)) ([#162](https://github.com/yaad-index/yaad-index/issues/162)) ([bbb17e5](https://github.com/yaad-index/yaad-index/commit/bbb17e538e022c4934715aa5f61e4d84a2a5a9f2))

## [0.5.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.4.0...yaad-index-v0.5.0) (2026-05-17)


### Features

* **workflow,actions:** add_canonical_edge primitive — workflow creates canonical edge + per-entry data directly ([#132](https://github.com/yaad-index/yaad-index/issues/132)) ([#136](https://github.com/yaad-index/yaad-index/issues/136)) ([09a3bb4](https://github.com/yaad-index/yaad-index/commit/09a3bb4f1bb4caefe97d139f8bf44ec008edb019))
* **workflow,actions:** add_gap carries full gap spec inline ([#142](https://github.com/yaad-index/yaad-index/issues/142)) ([#144](https://github.com/yaad-index/yaad-index/issues/144)) ([4bebe7c](https://github.com/yaad-index/yaad-index/commit/4bebe7cdcc0289024c9a350e0e353f10d1b566d7))


### Bug Fixes

* **api,fill:** honor workflow-injected canonical_type spec on fill paths ([#158](https://github.com/yaad-index/yaad-index/issues/158)) ([#159](https://github.com/yaad-index/yaad-index/issues/159)) ([182354f](https://github.com/yaad-index/yaad-index/commit/182354ff726cbd41dab9615f89636697b54f5a98))
* **api,needs_fill:** surface source-shape entities with workflow-injected gaps ([#156](https://github.com/yaad-index/yaad-index/issues/156)) ([#157](https://github.com/yaad-index/yaad-index/issues/157)) ([1f35bac](https://github.com/yaad-index/yaad-index/commit/1f35baca34e75529651d2f78b1be1893b46e5b9a))
* **api:** release per-entity write-lock before bus publish ([#154](https://github.com/yaad-index/yaad-index/issues/154)) ([#155](https://github.com/yaad-index/yaad-index/issues/155)) ([523ff13](https://github.com/yaad-index/yaad-index/commit/523ff138bce11f42a42ab99e8c49fc2fbca71311))
* **config:** GapSpec.Kinds decodes scalar + sequence + nil shapes uniformly ([#141](https://github.com/yaad-index/yaad-index/issues/141)) ([#143](https://github.com/yaad-index/yaad-index/issues/143)) ([f54c8a6](https://github.com/yaad-index/yaad-index/commit/f54c8a615d3b97e9bb3b1cb97229d57529306ade))
* **gmail:** cap source slug length so vault write doesn't exceed FS name limit ([#146](https://github.com/yaad-index/yaad-index/issues/146)) ([#149](https://github.com/yaad-index/yaad-index/issues/149)) ([28c6400](https://github.com/yaad-index/yaad-index/commit/28c6400aa24d868926bd3118814b933be6039f1f))
* **workflow:** action writers wait-with-timeout for write-lock per [#152](https://github.com/yaad-index/yaad-index/issues/152) ([#153](https://github.com/yaad-index/yaad-index/issues/153)) ([5adb043](https://github.com/yaad-index/yaad-index/commit/5adb043f96fd2ab54774120f5fa0d0fb795d2088))
* **workflow:** entity activation nests Data under `data` key for CEL access ([#145](https://github.com/yaad-index/yaad-index/issues/145)) ([#148](https://github.com/yaad-index/yaad-index/issues/148)) ([3202d1b](https://github.com/yaad-index/yaad-index/commit/3202d1bdfcf48bdb68b8da8c80d477de77f0b728))
* **workflow:** replace per-entity rate-limit backstop with structural cycle detection ([#147](https://github.com/yaad-index/yaad-index/issues/147)) ([#151](https://github.com/yaad-index/yaad-index/issues/151)) ([1735f6b](https://github.com/yaad-index/yaad-index/commit/1735f6b24bc81d6432f9dd2cd308b71662650890))

## [0.4.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.3.0...yaad-index-v0.4.0) (2026-05-17)


### Features

* **fill,vault:** canonical_type entries carry data → dataview append on target ([#119](https://github.com/yaad-index/yaad-index/issues/119)) ([#124](https://github.com/yaad-index/yaad-index/issues/124)) ([c08a11d](https://github.com/yaad-index/yaad-index/commit/c08a11d8f8b6e84ebf1872fcbe8204effe3a1212))
* **workflow,api:** add_gap carries per-key data_schema for canonical_type data extension ([#117](https://github.com/yaad-index/yaad-index/issues/117)) ([#130](https://github.com/yaad-index/yaad-index/issues/130)) ([b476f02](https://github.com/yaad-index/yaad-index/commit/b476f02699ecaa108b7e92187081e78b4421378d))
* **workflow,decision:** cel-go strings ext + regex_capture function ([#123](https://github.com/yaad-index/yaad-index/issues/123)) ([#127](https://github.com/yaad-index/yaad-index/issues/127)) ([9ce17ef](https://github.com/yaad-index/yaad-index/commit/9ce17ef62fc2c6d5ec0dc34cfa0bba325d517def))
* **workflow:** set_property action — direct frontmatter write ([#121](https://github.com/yaad-index/yaad-index/issues/121)) ([58d3e8c](https://github.com/yaad-index/yaad-index/commit/58d3e8c9b6cd5ec47bbe9bcf411c6a8c738dbc34))

## [0.3.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.2.0...yaad-index-v0.3.0) (2026-05-17)


### Features

* **api:** emit eventbus events on canonical-type fill edges ([#66](https://github.com/yaad-index/yaad-index/issues/66) phase 2.2.B.1) ([#75](https://github.com/yaad-index/yaad-index/issues/75)) ([e12d482](https://github.com/yaad-index/yaad-index/commit/e12d482998f0e1610ce7d34ace594eeff646440d))
* **api:** ingest tracker eventbus emissions with cache-hit detection ([#66](https://github.com/yaad-index/yaad-index/issues/66) phase 2.2.B.2) ([#76](https://github.com/yaad-index/yaad-index/issues/76)) ([fa4eb33](https://github.com/yaad-index/yaad-index/commit/fa4eb33ec215821bebad04b2c681d5bd1bbfeb71))
* **api:** UGC eventbus emissions + plugin_dispatch docstring fix ([#67](https://github.com/yaad-index/yaad-index/issues/67) phase 2.2.C) ([#88](https://github.com/yaad-index/yaad-index/issues/88)) ([3f8562a](https://github.com/yaad-index/yaad-index/commit/3f8562a16956b782775cf91a3598800f332320b4))
* **api:** wire eventbus + emit on /v1/edges + /fill + /operator-fill ([#66](https://github.com/yaad-index/yaad-index/issues/66) phase 2.2.A) ([#74](https://github.com/yaad-index/yaad-index/issues/74)) ([2864468](https://github.com/yaad-index/yaad-index/commit/2864468bca279a9ad7bd06d6353f450d0c055933))
* **eventbus:** substrate package for workflow engine v1 ([#66](https://github.com/yaad-index/yaad-index/issues/66) PR-A) ([#72](https://github.com/yaad-index/yaad-index/issues/72)) ([875363e](https://github.com/yaad-index/yaad-index/commit/875363efcbcb0eb8ce40a05422471340bd8c8b99))
* **workflow:** action runner substrate + task_append + Execute hook ([#69](https://github.com/yaad-index/yaad-index/issues/69) phase 4.A) ([#82](https://github.com/yaad-index/yaad-index/issues/82)) ([698ecf4](https://github.com/yaad-index/yaad-index/commit/698ecf4c45c7a1fc0e053bcf49de51bd2c66469b))
* **workflow:** add_comment + add_gap runner contracts ([#69](https://github.com/yaad-index/yaad-index/issues/69) phase 4.B / Path B) ([#83](https://github.com/yaad-index/yaad-index/issues/83)) ([c56a3ce](https://github.com/yaad-index/yaad-index/commit/c56a3ceffda0d712c93974433ba7c8fbb25b56b6))
* **workflow:** CEL template rendering (mustache + bare-CEL) ([#69](https://github.com/yaad-index/yaad-index/issues/69) PR-82 carry-over) ([#84](https://github.com/yaad-index/yaad-index/issues/84)) ([a1f5553](https://github.com/yaad-index/yaad-index/commit/a1f55534a7697554c73ecc6fe0ce6534820bd700))
* **workflow:** decision package with CEL evaluator ([#68](https://github.com/yaad-index/yaad-index/issues/68) phase 3.A) ([#79](https://github.com/yaad-index/yaad-index/issues/79)) ([ecfd137](https://github.com/yaad-index/yaad-index/commit/ecfd1377fbb8c3aa50ff071023c114160042dfa1))
* **workflow:** engine + bus subscription + main.go wiring ([#68](https://github.com/yaad-index/yaad-index/issues/68) phase 3.B) ([#80](https://github.com/yaad-index/yaad-index/issues/80)) ([e3c240e](https://github.com/yaad-index/yaad-index/commit/e3c240e0b748160d1679822d140b7955a7e8d829))
* **workflow:** engine runaway-fire backstop ([#70](https://github.com/yaad-index/yaad-index/issues/70) phase 5.D) ([#94](https://github.com/yaad-index/yaad-index/issues/94)) ([2d43225](https://github.com/yaad-index/yaad-index/commit/2d43225af783dc5bf807782733f5784fedb67790))
* **workflow:** err-task pattern ([#70](https://github.com/yaad-index/yaad-index/issues/70) phase 5.B) ([#92](https://github.com/yaad-index/yaad-index/issues/92)) ([9720336](https://github.com/yaad-index/yaad-index/commit/972033604688c8230230e344651d8a6aa01393ed))
* **workflow:** loader + registry with mtime hot-reload ([#67](https://github.com/yaad-index/yaad-index/issues/67) phase 1.B) ([#78](https://github.com/yaad-index/yaad-index/issues/78)) ([6e3e45e](https://github.com/yaad-index/yaad-index/commit/6e3e45e468e8ddb6c49cd26933d1d6101fd026ca))
* **workflow:** manual-trigger HTTP + CLI + PR-80 fold-ins ([#68](https://github.com/yaad-index/yaad-index/issues/68) phase 3.C) ([#81](https://github.com/yaad-index/yaad-index/issues/81)) ([495bc5a](https://github.com/yaad-index/yaad-index/commit/495bc5af04b353ffc70fe5036a5ae52d4782cbe4))
* **workflow:** missing-reference notes on tasks ([#70](https://github.com/yaad-index/yaad-index/issues/70) phase 5.C) ([#93](https://github.com/yaad-index/yaad-index/issues/93)) ([f237434](https://github.com/yaad-index/yaad-index/commit/f237434c7f4af4da7b5727d975bff9732a289d99))
* **workflow:** parser for operator-authored workflow files ([#67](https://github.com/yaad-index/yaad-index/issues/67) phase 1.A) ([#77](https://github.com/yaad-index/yaad-index/issues/77)) ([fd1920c](https://github.com/yaad-index/yaad-index/commit/fd1920cbaed11a79f6b3836298ab2fe875d01f9d))
* **workflow:** per-pattern dedup foundation ([#70](https://github.com/yaad-index/yaad-index/issues/70) phase 5.A) ([#91](https://github.com/yaad-index/yaad-index/issues/91)) ([3afe06b](https://github.com/yaad-index/yaad-index/commit/3afe06b4e242adfd286e9a793cdbcbf41feb4b1d))
* **workflow:** plugin_dispatch runner contract + stub ([#69](https://github.com/yaad-index/yaad-index/issues/69) phase 4.C / Path B) ([#85](https://github.com/yaad-index/yaad-index/issues/85)) ([2452326](https://github.com/yaad-index/yaad-index/commit/245232669c5034e054889afcadc6698833a055f9))
* **workflow:** registry-backed PluginDispatcher real impl ([#69](https://github.com/yaad-index/yaad-index/issues/69) phase 4.C.2) ([#87](https://github.com/yaad-index/yaad-index/issues/87)) ([50c5a41](https://github.com/yaad-index/yaad-index/commit/50c5a4143fa56a69cb881696321fd3190e3cbf3f))
* **workflow:** task.list + task.load surface ([#71](https://github.com/yaad-index/yaad-index/issues/71) phase 6.B) ([#96](https://github.com/yaad-index/yaad-index/issues/96)) ([b9acb02](https://github.com/yaad-index/yaad-index/commit/b9acb02aaad28efccaa17ec3263aec73a29c4979))
* **workflow:** task.resolve + E2E agent surface test ([#71](https://github.com/yaad-index/yaad-index/issues/71) phase 6.C) ([#97](https://github.com/yaad-index/yaad-index/issues/97)) ([1b7add2](https://github.com/yaad-index/yaad-index/commit/1b7add21ea2f359a05c3c244f058e473f52983b0))
* **workflow:** URL → ingest-or-lookup routing in Engine.Dispatch ([#68](https://github.com/yaad-index/yaad-index/issues/68) carry-over) ([#89](https://github.com/yaad-index/yaad-index/issues/89)) ([915be1e](https://github.com/yaad-index/yaad-index/commit/915be1e8d1b794f13ac2f67d91adf0426cd1f9b1))
* **workflow:** vault-backed CommentWriter + GapWriter ([#69](https://github.com/yaad-index/yaad-index/issues/69) phase 4.B.2) ([#86](https://github.com/yaad-index/yaad-index/issues/86)) ([70fd20f](https://github.com/yaad-index/yaad-index/commit/70fd20fddd1ef8499ddaacea5afa2f9e09825625))
* **workflow:** workflow.list + workflow.discover surface ([#71](https://github.com/yaad-index/yaad-index/issues/71) phase 6.A) ([#95](https://github.com/yaad-index/yaad-index/issues/95)) ([e929f94](https://github.com/yaad-index/yaad-index/commit/e929f94eb6faa152a6b75a0219c8e4ed054ab749))


### Bug Fixes

* **api:** needs_fill scans candidates until found-or-exhausted per call ([#114](https://github.com/yaad-index/yaad-index/issues/114)) ([fb643d6](https://github.com/yaad-index/yaad-index/commit/fb643d64dde377a2abfcec0e7a6368336b333a3e))
* **config:** canonical_kinds validator allows hyphenated kind names ([#103](https://github.com/yaad-index/yaad-index/issues/103)) ([641c023](https://github.com/yaad-index/yaad-index/commit/641c023b160c6255be61d8ce01f3c6df029c9fc7))
* **search:** drop EntityKinds allowlist on kind filter ([#111](https://github.com/yaad-index/yaad-index/issues/111)) ([0454acd](https://github.com/yaad-index/yaad-index/commit/0454acd72b6c68ad688ea834eee735494b4e00b2))
* **subprocess:** bump fetchTimeout default to 60s + add per-plugin config knob ([#108](https://github.com/yaad-index/yaad-index/issues/108)) ([148982d](https://github.com/yaad-index/yaad-index/commit/148982d89ece87aa0e4ad5e7f5e5f64f1e91b9d6))
* **subprocess:** SIGTERM + grace window so timed-out plugins flush buffered envelopes ([#109](https://github.com/yaad-index/yaad-index/issues/109)) ([5c680c4](https://github.com/yaad-index/yaad-index/commit/5c680c47bc9193bdd4f1456e61ca6fc717ac8706))
* **workflow/loader:** collision-rejected files re-attempt after prior removed ([#90](https://github.com/yaad-index/yaad-index/issues/90)) ([38729a0](https://github.com/yaad-index/yaad-index/commit/38729a03a6aee1c8e9edbcd16fdd0b565398a14b))

## [0.2.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.1.0...yaad-index-v0.2.0) (2026-05-15)


### Features

* **api:** per-entity write-lock with block-on-conflict (closes [#23](https://github.com/yaad-index/yaad-index/issues/23)) ([#29](https://github.com/yaad-index/yaad-index/issues/29)) ([a1c8724](https://github.com/yaad-index/yaad-index/commit/a1c8724921c8e255360477bc51af6fd195fa5637))
* **cache:** refetch --wait-seconds flag + queued-state output ([#6](https://github.com/yaad-index/yaad-index/issues/6)) ([#42](https://github.com/yaad-index/yaad-index/issues/42)) ([7e09343](https://github.com/yaad-index/yaad-index/commit/7e093430ffa2795ec73f09b187e70788826e2e4f))
* **notes:** marker-pair preservation per ADR-0015 pattern ([#8](https://github.com/yaad-index/yaad-index/issues/8)) ([#37](https://github.com/yaad-index/yaad-index/issues/37)) ([1f9791f](https://github.com/yaad-index/yaad-index/commit/1f9791f959757491779a92b4a6f625b993e2b571))
* **e2e:** multi-plugin ingest harness + wikipedia happy path ([#1](https://github.com/yaad-index/yaad-index/issues/1) PR-A) ([#51](https://github.com/yaad-index/yaad-index/issues/51)) ([bea039a](https://github.com/yaad-index/yaad-index/commit/bea039a91f26c8e873a37327d894e72262cae724))
* **gmail:** MIME-walk + ADR-0014 per-envelope attachment staging ([#12](https://github.com/yaad-index/yaad-index/issues/12)) ([#35](https://github.com/yaad-index/yaad-index/issues/35)) ([f5a4422](https://github.com/yaad-index/yaad-index/commit/f5a4422e17258cb5f4e61bd98519b35567817253))
* **needs-fill:** canonical-kind registry as gap-prompt source ([#4](https://github.com/yaad-index/yaad-index/issues/4)) ([#39](https://github.com/yaad-index/yaad-index/issues/39)) ([8391bd6](https://github.com/yaad-index/yaad-index/commit/8391bd6779f752bc89ef0e2e6885805906b71c2d))
* **plugins:** operator-yaml config → spawn env vars ([#7](https://github.com/yaad-index/yaad-index/issues/7)) ([#38](https://github.com/yaad-index/yaad-index/issues/38)) ([a883004](https://github.com/yaad-index/yaad-index/commit/a8830047741f3ea20127b324c4a5e260b21e8843))
* **reindex:** clear drift counters on successful reindex ([#31](https://github.com/yaad-index/yaad-index/issues/31)) ([#46](https://github.com/yaad-index/yaad-index/issues/46)) ([1410179](https://github.com/yaad-index/yaad-index/commit/1410179e623aded9c7b986e79c7c11aa93d20f6e))
* **search:** POST /v1/search/upstream + plugin Search contract ([#2](https://github.com/yaad-index/yaad-index/issues/2) PR-1) ([#49](https://github.com/yaad-index/yaad-index/issues/49)) ([5d8cf89](https://github.com/yaad-index/yaad-index/commit/5d8cf890119659c537f029d6aa9795980ac7d64a))


### Bug Fixes

* **api:** route command-shape dispatch via LookupByName ([#52](https://github.com/yaad-index/yaad-index/issues/52)) ([#53](https://github.com/yaad-index/yaad-index/issues/53)) ([5b186f0](https://github.com/yaad-index/yaad-index/commit/5b186f0136ea5bc0826926b658b20f719add24b9))
* **canonical-guard:** auto-activate plugin-emitted edge types ([#9](https://github.com/yaad-index/yaad-index/issues/9)) ([#32](https://github.com/yaad-index/yaad-index/issues/32)) ([39d528e](https://github.com/yaad-index/yaad-index/commit/39d528eff3c608d679600fdb9d06738046186d8a))
* **gmail:** de-duplicate FETCH responses by UID ([#60](https://github.com/yaad-index/yaad-index/issues/60)) ([#61](https://github.com/yaad-index/yaad-index/issues/61)) ([4d605ef](https://github.com/yaad-index/yaad-index/commit/4d605ef251fe0cc1cdfc9a6713b990ad1a6c464d))
* **gmail:** debug logging on poll Tick success path ([#54](https://github.com/yaad-index/yaad-index/issues/54)) ([#55](https://github.com/yaad-index/yaad-index/issues/55)) ([fdf4521](https://github.com/yaad-index/yaad-index/commit/fdf4521030ad0ade1eeac9e297cb67c56e399023))
* **gmail:** surface io.ReadAll errors on FETCH body ([#58](https://github.com/yaad-index/yaad-index/issues/58)) ([#59](https://github.com/yaad-index/yaad-index/issues/59)) ([2d5d8c7](https://github.com/yaad-index/yaad-index/commit/2d5d8c76d16966721b423edbdd3ea29a15f00c89))
* **gmail:** X-GM-RAW search wire shape ([#56](https://github.com/yaad-index/yaad-index/issues/56)) ([#57](https://github.com/yaad-index/yaad-index/issues/57)) ([bda9518](https://github.com/yaad-index/yaad-index/commit/bda951890591863149e3f6e9c8fc3321869dda83))
* **staging-dir:** three-layer resolution chain + os.TempDir() default ([#33](https://github.com/yaad-index/yaad-index/issues/33)) ([#34](https://github.com/yaad-index/yaad-index/issues/34)) ([be83006](https://github.com/yaad-index/yaad-index/commit/be83006da8055e9865c8a8e8af50a3b18bb90a9e))
