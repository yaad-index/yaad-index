# Changelog

## [0.2.0](https://github.com/yaad-index/yaad-index/compare/yaad-index-v0.1.0...yaad-index-v0.2.0) (2026-05-15)


### Features

* **api:** per-entity write-lock with block-on-conflict (closes [#23](https://github.com/yaad-index/yaad-index/issues/23)) ([#29](https://github.com/yaad-index/yaad-index/issues/29)) ([a1c8724](https://github.com/yaad-index/yaad-index/commit/a1c8724921c8e255360477bc51af6fd195fa5637))
* **cache:** refetch --wait-seconds flag + queued-state output ([#6](https://github.com/yaad-index/yaad-index/issues/6)) ([#42](https://github.com/yaad-index/yaad-index/issues/42)) ([7e09343](https://github.com/yaad-index/yaad-index/commit/7e093430ffa2795ec73f09b187e70788826e2e4f))
* **comments:** marker-pair preservation per ADR-0015 pattern ([#8](https://github.com/yaad-index/yaad-index/issues/8)) ([#37](https://github.com/yaad-index/yaad-index/issues/37)) ([1f9791f](https://github.com/yaad-index/yaad-index/commit/1f9791f959757491779a92b4a6f625b993e2b571))
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
