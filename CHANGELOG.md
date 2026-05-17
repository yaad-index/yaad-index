# Changelog

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
