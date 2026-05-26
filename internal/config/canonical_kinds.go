package config

import (
	"fmt"
	"log/slog"

	"gopkg.in/yaml.v3"
)

// GapSpec describes a single gap field in the canonical-kind
// registry per ADR-0016 §7 + ADR-0019 §Per-gap fill-strategy hint.
// Three YAML shapes parse equivalently:
//
//	# Shorthand (string-typed) — backward-compatible with the
//	# pre-ADR-0016 `gaps: map[string]string` shape:
//	gaps:
//	 name: "Full name"
//
//	# Long form (typed, ADR-0016):
//	gaps:
//	 birthdate:
//	 type: date
//	 description: "Birth date in YYYY-MM-DD if known."
//
//	# Long form with ADR-0019 type-and-strategy metadata:
//	gaps:
//	 rating:
//	 prompt: "How do you rate this?" # alias for description
//	 type: int
//	 range: [1, 10]
//	 fill_strategy: operator
//	 played:
//	 type: bool
//	 fill_strategy: operator
//	 region:
//	 type: enum
//	 values: ["us", "eu", "asia"]
//	 fill_strategy: agent
//
// All shapes round-trip to GapSpec. Description and Prompt are
// aliases — the long form's `prompt:` key reads into Description so
// the same downstream code path drives fill prompt construction
// regardless of which spelling the operator used. ADR-0019 added
// the `prompt` spelling to match the operator-fill semantic; the
// agent-side ADR-0016 prose used `description`. Both are valid;
// the daemon doesn't distinguish.
//
// Type defaults to "string" when the shorthand path is taken or
// when the long form omits it. FillStrategy defaults to "both"
// (the agent attempts first per the existing flow; operator picks
// up gaps the agent leaves).
//
// Type validation:
// - "int" → optional Range[2]int{min, max}; Min ≤ Max required.
// - "bool" → no extra fields.
// - "string" → optional MaxLength > 0.
// - "text" → no extra fields. Distinct from "string" only at
// the prompt-construction layer (multi-line prose vs short
// value).
// - "enum" → required Values []string non-empty.
//
// Validation runs at config Load time; type/range/values mismatches
// fail server start so operator typos are caught before any agent
// or operator hits the gap.
type GapSpec struct {
	Type string `yaml:"type" json:"type,omitempty"`
	Description string `yaml:"description" json:"description"`
	FillStrategy string `yaml:"fill_strategy" json:"fill_strategy,omitempty"`
	Range []int `yaml:"range,omitempty" json:"range,omitempty"`
	MaxLength int `yaml:"max_length,omitempty" json:"max_length,omitempty"`
	Values []string `yaml:"values,omitempty" json:"values,omitempty"`
	// Kinds restricts the canonical kinds a `type: canonical_type`
	// gap accepts at fill time per yaad-index. Two shapes:
	//
	// - `["person", "boardgame"]` — explicit kind allowlist; only
	// fills whose elements declare one of these kinds pass.
	// - `["*"]` — operator's full canonical_kinds registry per
	// ADR-0008. Validated at fill-time against the resolved
	// registry, NOT at config-load (the registry is a runtime
	// concept).
	//
	// On the wire (YAML + JSON), the field is polymorphic: a bare
	// scalar `"*"` is accepted alongside the list shape and decodes
	// to the canonical wildcard `[]string{"*"}`. UnmarshalYAML and
	// the plugin-side JSON decoder handle the polymorphism so
	// downstream code reads `Kinds` as a `[]string` uniformly.
	//
	// Empty / nil → the gap is NOT a canonical_type. Required when
	// Type=="canonical_type"; rejected otherwise (Validate returns
	// an error).
	Kinds []string `yaml:"-" json:"kinds,omitempty"`
}

// gapSpecYAML is the long-form decode target. Mirror of GapSpec
// plus the `prompt` alias so YAML's case-insensitive struct match
// doesn't conflict with the existing `description`. Two distinct
// fields → unambiguous decode → merge logic in UnmarshalYAML
// picks whichever was set.
//
// Kinds uses the typed kindsYAML shape so the polymorphic field
// (scalar `"*"` vs sequence `["person", "boardgame"]`) decodes
// uniformly via kindsYAML.UnmarshalYAML — yaml.v3 v3.0.1 cannot
// decode either shape into a bare `*yaml.Node` field (#141), so
// we route through a custom unmarshaler instead.
type gapSpecYAML struct {
	Type string `yaml:"type"`
	Description string `yaml:"description"`
	Prompt string `yaml:"prompt"`
	FillStrategy string `yaml:"fill_strategy"`
	Range []int `yaml:"range"`
	MaxLength int `yaml:"max_length"`
	Values []string `yaml:"values"`
	Kinds kindsYAML `yaml:"kinds"`
}

// kindsYAML decodes the polymorphic `kinds:` field. Two accepted
// shapes per ADR-0021 + yaad-index:
//
//   - Scalar `"*"` → `[]string{"*"}` (wildcard sentinel).
//   - Sequence `[person, boardgame]` (block or flow) → verbatim.
//
// Absent / nil decodes to nil. Any other shape (mapping, nested
// sequence, non-scalar list entries) rejects at config-load time.
//
// The custom UnmarshalYAML bypasses the yaml.v3 v3.0.1
// `*yaml.Node` decode bug (#141): the library can't unmarshal
// !!seq or !!str into a yaml.Node struct field, but it routes
// through this method's `*yaml.Node` parameter cleanly.
type kindsYAML []string

func (k *kindsYAML) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Kind == 0 {
		return nil
	}
	switch value.Kind {
	case yaml.ScalarNode:
		// Bare scalar → single-entry slice. Validation downstream
		// enforces that "*" is the only legal scalar.
		*k = []string{value.Value}
		return nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(value.Content))
		for _, c := range value.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("kinds list entries must be scalars, got kind %d", c.Kind)
			}
			out = append(out, c.Value)
		}
		*k = out
		return nil
	default:
		return fmt.Errorf("kinds must be a scalar `\"*\"` or a list of strings, got kind %d", value.Kind)
	}
}

// CanonicalTypeName is the GapSpec.Type sentinel for the
// canonical_type gap shape per yaad-index: a list-valued
// gap whose elements are canonical entity references. The
// daemon validates fills against the gap's `kinds` allowlist
// (or the operator's full canonical_kinds registry when
// kinds == ["*"]) and creates edges from the source entity to
// each canonical label produced by the fill.
const CanonicalTypeName = "canonical_type"

// CanonicalTypeWildcard is the sentinel kinds entry that means
// "any canonical kind in the operator's canonical_kinds
// registry" (per ADR-0008). Resolved at fill time, not at
// config-load (the registry is a runtime concept). Validate
// rejects mixing the wildcard with explicit kind names.
const CanonicalTypeWildcard = "*"

// UnmarshalYAML accepts the string-shorthand AND struct-long-form
// shapes per ADR-0016 §7 + ADR-0019. Required so existing operator
// configs using the pre-ADR-0016 `gaps: map[string]string` continue
// to parse without rewrite.
func (g *GapSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode && (value.Tag == "!!str" || value.Tag == "") {
		g.Type = "string"
		g.Description = value.Value
		return nil
	}
	var p gapSpecYAML
	if err := value.Decode(&p); err != nil {
		return fmt.Errorf("decode GapSpec: %w", err)
	}
	if p.Type == "" {
		p.Type = "string"
	}
	desc := p.Description
	if desc == "" {
		desc = p.Prompt
	}
	*g = GapSpec{
		Type: p.Type,
		Description: desc,
		FillStrategy: p.FillStrategy,
		Range: p.Range,
		MaxLength: p.MaxLength,
		Values: p.Values,
		Kinds: []string(p.Kinds),
	}
	return nil
}

// Valid GapSpec.FillStrategy values per ADR-0019 §Per-gap
// fill-strategy hint. Empty string is treated as the default
// `both` at Validate time so existing configs (no fill_strategy
// declared) parse unchanged.
var validFillStrategies = map[string]struct{}{
	"": {}, // → "both" default applied by callers
	"agent": {},
	"operator": {},
	"both": {},
}

// Validate enforces ADR-0019 typed-gap rules at parse time. Called
// from validateCanonicalKindConfig for each gap entry. Path names
// the gap field for error messages.
//
// Type policy: ADR-0019 names a closed set of well-known types
// (int, bool, string, text, enum) that get type-aware extra-field
// validation. Pre-ADR-0019 configs may declare other strings (the
// historical example was "date" — descriptive-only per ADR-0016
// §7). Those pass through as-is so existing operator configs keep
// loading; any extra fields under a non-well-known type are
// accepted without enforcement (the daemon treats them as opaque
// hints surfaced to the operator at fill prompt time).
func (g GapSpec) Validate(path string) error {
	if _, ok := validFillStrategies[g.FillStrategy]; !ok {
		return fmt.Errorf("%s.fill_strategy=%q not in {agent, operator, both}", path, g.FillStrategy)
	}
	if g.Type != CanonicalTypeName && len(g.Kinds) > 0 {
		return fmt.Errorf("%s: kinds is only valid when type=%q (got type=%q)",
			path, CanonicalTypeName, g.Type)
	}
	switch g.Type {
	case CanonicalTypeName:
		if len(g.Kinds) == 0 {
			return fmt.Errorf("%s: type=%q requires non-empty kinds list (allowlist of canonical kinds, or [%q] for any)",
				path, CanonicalTypeName, CanonicalTypeWildcard)
		}
		// Wildcard alone (`["*"]`). Mixing the wildcard with
		// explicit kinds is rejected — declare one OR the other.
		hasWildcard := false
		for _, k := range g.Kinds {
			if k == "" {
				return fmt.Errorf("%s.kinds: empty string not allowed", path)
			}
			if k == CanonicalTypeWildcard {
				hasWildcard = true
			}
		}
		if hasWildcard && len(g.Kinds) != 1 {
			return fmt.Errorf("%s.kinds: wildcard %q must appear alone (got %v)",
				path, CanonicalTypeWildcard, g.Kinds)
		}
		if g.MaxLength != 0 || len(g.Range) > 0 || len(g.Values) > 0 {
			return fmt.Errorf("%s: type=%q does not accept max_length, range, or values",
				path, CanonicalTypeName)
		}
	case "int":
		if len(g.Range) > 0 {
			if len(g.Range) != 2 {
				return fmt.Errorf("%s.range must be [min, max] (length 2), got length %d", path, len(g.Range))
			}
			if g.Range[0] > g.Range[1] {
				return fmt.Errorf("%s.range: min %d > max %d", path, g.Range[0], g.Range[1])
			}
		}
		if g.MaxLength != 0 || len(g.Values) > 0 {
			return fmt.Errorf("%s: type=int does not accept max_length or values", path)
		}
	case "string":
		if g.MaxLength < 0 {
			return fmt.Errorf("%s.max_length=%d is negative", path, g.MaxLength)
		}
		if len(g.Range) > 0 || len(g.Values) > 0 {
			return fmt.Errorf("%s: type=string does not accept range or values", path)
		}
	case "text", "bool":
		if g.MaxLength != 0 || len(g.Range) > 0 || len(g.Values) > 0 {
			return fmt.Errorf("%s: type=%s does not accept max_length, range, or values", path, g.Type)
		}
	case "enum":
		if len(g.Values) == 0 {
			return fmt.Errorf("%s: type=enum requires non-empty values list", path)
		}
		seen := make(map[string]struct{}, len(g.Values))
		for _, v := range g.Values {
			if v == "" {
				return fmt.Errorf("%s.values: empty string not allowed", path)
			}
			if _, dup := seen[v]; dup {
				return fmt.Errorf("%s.values: duplicate %q", path, v)
			}
			seen[v] = struct{}{}
		}
		if g.MaxLength != 0 || len(g.Range) > 0 {
			return fmt.Errorf("%s: type=enum does not accept max_length or range", path)
		}
	default:
		// Unknown / pre-ADR-0019 type. Pass through without extra-
		// field enforcement (legacy types like "date" are
		// descriptive-only per ADR-0016 §7).
	}
	return nil
}

// InstructionSpec is the AI-fill instruction per ADR-0016 §1+§4.
// Operator-only end-to-end — the plugin layer is forbidden from
// declaring this struct (per ADR-0016 §2 + the daemon's WARN-and-
// ignore behavior on plugin-supplied instruction fields).
//
// Two YAML shapes parse equivalently:
//
//	# Shorthand (bare string) — pre-ADR-0016 backward-compat.
//	# Treated as enabled=true: the operator wrote prose, they
//	# want it driving fill.
//	instruction: "Fill carefully. Cite sources where possible."
//
//	# Long form:
//	instruction:
//	 enabled: true
//	 text: "..."
//
// Both round-trip to InstructionSpec{Enabled, Text}. Empty Text +
// Enabled=true is a configuration mistake; the daemon's fill path
// emits an INFO log when this combination is encountered ("set
// instruction.text in operator config").
type InstructionSpec struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Text string `yaml:"text" json:"text,omitempty"`
}

// UnmarshalYAML accepts the string-shorthand AND struct-long-form
// shapes per ADR-0016 §1. Required so existing operator configs
// using the pre-ADR-0016 `instruction: "string"` shape continue
// to parse with the migration semantic
// (Enabled=true, Text=<the string>).
func (i *InstructionSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode && (value.Tag == "!!str" || value.Tag == "") {
		// Pre-ADR-0016 shape. The operator wrote prose; treat as
		// enabled. They opt out via the long-form
		// `instruction: {enabled: false}` if they want the prose
		// stored without driving fill.
		i.Enabled = true
		i.Text = value.Value
		return nil
	}
	type plainInstruction InstructionSpec
	var p plainInstruction
	if err := value.Decode(&p); err != nil {
		return fmt.Errorf("decode InstructionSpec: %w", err)
	}
	*i = InstructionSpec(p)
	return nil
}

// GapsFromMap is a test/migration helper that lifts a legacy
// `map[string]string` (description-only) into the typed
// `map[string]GapSpec` shape ADR-0016 §7 introduces. Each entry
// becomes a string-typed GapSpec carrying the original
// description verbatim.
//
// Used by:
// - Pre-ADR-0016 tests constructing CanonicalKindConfig literals.
// - Future migration helpers that convert wire-shape (legacy
// flat) to the internal typed shape.
//
// Returns an empty (non-nil) map for an empty input — matches
// CanonicalKindConfig's "Gaps may be empty" contract.
func GapsFromMap(m map[string]string) map[string]GapSpec {
	out := make(map[string]GapSpec, len(m))
	for k, v := range m {
		out[k] = GapSpec{Type: "string", Description: v}
	}
	return out
}

// InstructionFromString is the test/migration counterpart of
// GapsFromMap: lifts a bare string into the *InstructionSpec
// shape ADR-0016 §1 introduces. Mirrors the YAML shorthand decode
// (Enabled=true, Text=s) — operator wrote prose, prose drives
// fill. Returns nil for empty input so the caller can express
// "no instruction at this layer" cleanly.
func InstructionFromString(s string) *InstructionSpec {
	if s == "" {
		return nil
	}
	return &InstructionSpec{Enabled: true, Text: s}
}

// DefaultGaps returns the built-in gap-set every canonical kind
// has by default per ADR-0016 §1. The three gaps are present for
// every kind regardless of plugin or operator config; they cannot
// be removed (they're the columns the DB indexes for /v1/search).
// Their Description CAN be overridden by plugin-extras or
// operator-config layers.
//
// Returns a fresh copy on each call so callers can mutate the
// returned map without aliasing the constant data.
func DefaultGaps() map[string]GapSpec {
	return map[string]GapSpec{
		"name": {Type: "string", Description: "The name of the entity."},
		"tags": {Type: "[]string", Description: "Relevant tags for this entity."},
		"summary": {Type: "string", Description: "A short prose summary."},
	}
}

// DefaultInstruction returns the built-in instruction defaults per
// ADR-0016 §1. Both fields zero — operator config is the SOLE
// layer that contributes to the effective instruction.text and
// instruction.enabled. Plugins are forbidden from declaring
// instruction at all.
func DefaultInstruction() InstructionSpec {
	return InstructionSpec{Enabled: false, Text: ""}
}

// BuiltinKindGaps returns kind-specific built-in gap defaults per
// ADR-0019 step 4 + #48 slice 2. Layered between the universal
// DefaultGaps() and plugin extras during merge — a slot reserved
// for gaps that belong to the canonical-kind shape itself (e.g.
// an operator's rating/owned/want/played for boardgame), shipped
// with the daemon rather than the plugin so an operator running
// with no BGG plugin still gets the right gap surface on a
// boardgame entity.
//
// Returns a fresh copy so callers can mutate without aliasing.
// Empty map for kinds with no built-in extras.
//
// Built-in shapes shipped today (per #48 slice 2 starter-pool):
//
//   - boardgame: rating / owned / want / played / knows_how_to_play
//     (per ADR-0019 §Decision).
//   - person: birth_date / death_date / occupation.
//   - place: country / type (enum: city / country / region /
//     landmark / neighborhood / other).
//   - book: author / year / rating / read.
//   - article: author / publication / published_date.
//   - recipe: cuisine / prep_time_minutes / servings.
//
// **Dormant-until-active.** A kind's built-in gaps surface only
// when the kind activates in the merged registry — either a
// plugin's `canonical_kinds_emitted` triggers it or operator
// config explicitly lists it under `canonical_kinds:`. Layer 1.5
// is a starter pool, not auto-on (preserves ADR-0013's opt-in
// canonical-kind contract).
//
// Operator config can override any field of a built-in by
// declaring it under `canonical_kinds.<kind>.gaps.<field>` with
// new type / description / range / etc. — the merge's last-
// write-wins replaces the built-in's spec with the operator's.
// Per-gap disable (drop the built-in entirely without supplying
// a replacement spec) is NOT supported in v1 — the validator
// rejects empty / whitespace-only Description, so the "declare
// with empty description to wipe" pattern fails config-load
// rather than disabling. Disable lands in a follow-up if real
// operator pain surfaces.
func BuiltinKindGaps(kind string) map[string]GapSpec {
	switch kind {
	case "boardgame":
		return map[string]GapSpec{
			"rating": {
				Type: "int",
				Description: "How do you rate this on a 1-10 scale?",
				Range: []int{1, 10},
				FillStrategy: "operator",
			},
			"owned": {
				Type: "bool",
				Description: "Do you own this?",
				FillStrategy: "operator",
			},
			"want": {
				Type: "bool",
				Description: "Do you want this?",
				FillStrategy: "operator",
			},
			"played": {
				Type: "bool",
				Description: "Have you played this?",
				FillStrategy: "operator",
			},
			"knows_how_to_play": {
				Type: "bool",
				Description: "Do you know how to play this?",
				FillStrategy: "operator",
			},
		}
	case "person":
		// Identity + biographical anchors. agent fills are
		// derived from clean_content when present; operator
		// picks up empty fills.
		return map[string]GapSpec{
			"birth_date": {
				Type:        "string",
				Description: "Date of birth (YYYY-MM-DD when known, year-only otherwise).",
				MaxLength:   32,
			},
			"death_date": {
				Type:        "string",
				Description: "Date of death (YYYY-MM-DD when known, year-only otherwise). Empty for living people.",
				MaxLength:   32,
			},
			"occupation": {
				Type:        "string",
				Description: "Primary occupation or role.",
				MaxLength:   128,
			},
		}
	case "place":
		// Geographic anchors. Both fields are agent-derivable
		// from clean_content when an upstream source includes
		// location prose.
		return map[string]GapSpec{
			"country": {
				Type:        "string",
				Description: "Country this place is in.",
				MaxLength:   64,
			},
			"type": {
				Type:        "enum",
				Description: "Kind of place.",
				Values:      []string{"city", "country", "region", "landmark", "neighborhood", "other"},
			},
		}
	case "book":
		// Bibliographic identity + the operator-judgment pair
		// for read-state tracking. rating mirrors boardgame's
		// shape so operators can lean the same way.
		return map[string]GapSpec{
			"author": {
				Type:        "string",
				Description: "Primary author.",
				MaxLength:   128,
			},
			"year": {
				Type:        "int",
				Description: "Year of first publication.",
				Range:       []int{0, 9999},
			},
			"rating": {
				Type:         "int",
				Description:  "How do you rate this on a 1-10 scale?",
				Range:        []int{1, 10},
				FillStrategy: "operator",
			},
			"read": {
				Type:         "bool",
				Description:  "Have you read this?",
				FillStrategy: "operator",
			},
		}
	case "article":
		// Bibliographic anchors for written pieces. All three
		// agent-derive cleanly from upstream metadata when
		// present (e.g. yaad-wikipedia, gmail newsletters).
		return map[string]GapSpec{
			"author": {
				Type:        "string",
				Description: "Primary author.",
				MaxLength:   128,
			},
			"publication": {
				Type:        "string",
				Description: "Publication or outlet.",
				MaxLength:   128,
			},
			"published_date": {
				Type:        "string",
				Description: "Publication date (YYYY-MM-DD when known).",
				MaxLength:   32,
			},
		}
	case "recipe":
		// Cooking-card anchors. Agent fills from upstream
		// content; operator can override per their household.
		return map[string]GapSpec{
			"cuisine": {
				Type:        "string",
				Description: "Cuisine type.",
				MaxLength:   64,
			},
			"prep_time_minutes": {
				Type:        "int",
				Description: "Preparation time in minutes.",
				Range:       []int{0, 1440},
			},
			"servings": {
				Type:        "int",
				Description: "Number of servings.",
				Range:       []int{1, 100},
			},
		}
	default:
		return map[string]GapSpec{}
	}
}

// LegacyCanonicalKindConfig is the pre-ADR-0016 wire shape (flat
// gaps: map[string]string + instruction: string) preserved on
// API responses for backward-compat with yaad-mcp clients that
// haven't migrated to the typed shape yet. Internal handlers
// project the post-ADR-0016 typed shape to this on JSON emit.
//
// Future PR migrates yaad-mcp + the wire to the typed shape; this
// type is the bridge until then.
type LegacyCanonicalKindConfig struct {
	Gaps map[string]string `json:"gaps"`
	Instruction string `json:"instruction,omitempty"`
}

// ToLegacyWireShape projects a CanonicalKindConfig (post-ADR-0016
// typed shape) to the legacy flat wire shape consumed by current
// yaad-mcp clients. Each gap loses its Type hint and surfaces only
// the Description; the Instruction struct collapses to its Text
// (regardless of Enabled — pre-ADR-0016 clients never knew about
// enabled).
func (c CanonicalKindConfig) ToLegacyWireShape() LegacyCanonicalKindConfig {
	gaps := make(map[string]string, len(c.Gaps))
	for k, spec := range c.Gaps {
		gaps[k] = spec.Description
	}
	instr := ""
	if c.Instruction != nil {
		instr = c.Instruction.Text
	}
	return LegacyCanonicalKindConfig{Gaps: gaps, Instruction: instr}
}

// LegacyRegistryWireShape projects an entire registry map to the
// legacy wire shape. Common shape on `/v1/structure` +
// `/v1/needs_fill` + ingest needs_fill responses.
func LegacyRegistryWireShape(reg map[string]CanonicalKindConfig) map[string]LegacyCanonicalKindConfig {
	out := make(map[string]LegacyCanonicalKindConfig, len(reg))
	for kind, cfg := range reg {
		out[kind] = cfg.ToLegacyWireShape()
	}
	return out
}

// MergeCanonicalRegistry computes the effective per-kind registry
// per ADR-0016 §4's four-layer merge:
//
// 1. Code defaults (DefaultGaps, DefaultInstruction).
// 2. Plugin extras: pluginGaps[kind][field] adds/overrides Layer 1.
// pluginEmittedKinds names the kind set every active plugin
// declares — these kinds are AVAILABLE in the registry without
// the operator re-enabling them.
// 3. Operator-defaults (root-scoped): opDefaults gaps add/override
// across every kind; opDefaults.Instruction is the root
// instruction applied unless overridden per-kind.
// 4. Operator per-kind: opPerKind[kind] gaps add/override the
// specific kind; opPerKind[kind].Instruction overrides
// opDefaults.Instruction for the kinds it covers.
//
// opPerKind's keys also activate kinds not in pluginEmittedKinds
// (operator-only kind for an entity whose plugin doesn't emit it
// canonically — rare but supported).
//
// Plugin-side gap-description conflicts (two plugins declare
// different descriptions for the same kind+field) are
// silently last-loaded-wins in this function — surfacing them
// as WARN logs requires per-plugin field provenance tracking
// (which plugin contributed each field), follow-up scope per
// ADR-0016 §5.
//
// The result is a map keyed by canonical-kind name; each value
// has a fully-resolved Gaps map (built-in + extras + operator
// overrides) and a concrete Instruction struct.
//
// Instruction merge IS NOT four-layer; it's two-layer (operator-
// defaults → operator per-kind) per ADR-0016 §4 last paragraph.
// Plugins are forbidden from declaring instruction; the daemon's
// capabilities parser strips any plugin-supplied instruction with
// a WARN before this function ever sees it.
func MergeCanonicalRegistry(
	pluginGaps map[string]map[string]GapSpec,
	pluginEmittedKinds []string,
	opDefaults CanonicalKindConfig,
	opPerKind map[string]CanonicalKindConfig,
	logger *slog.Logger,
) map[string]CanonicalKindConfig {
	merged, _ := MergeCanonicalRegistryWithProvenance(pluginGaps, pluginEmittedKinds, opDefaults, opPerKind, logger)
	return merged
}

// LayerProvenance names a layer in the canonical-registry merge.
// Returned per gap by MergeCanonicalRegistryWithProvenance so
// callers (per #48 slice 3 — `/v1/canonical_registry/effective`)
// can show operators which layer supplied each spec.
type LayerProvenance string

const (
	// LayerUniversalDefaults is the universal `DefaultGaps()`
	// layer — name / summary / tags. Applies to every active
	// kind.
	LayerUniversalDefaults LayerProvenance = "code_defaults"

	// LayerBuiltinKindGaps is the kind-specific daemon-shipped
	// gap-set from `BuiltinKindGaps(kind)` per #48 slice 2.
	// Layered above the universal defaults; below plugin extras.
	LayerBuiltinKindGaps LayerProvenance = "builtin_kind"

	// LayerPluginExtras is the plugin's `canonical_kinds_extras`
	// declared in `--init` (ADR-0016 §2 Layer 2).
	LayerPluginExtras LayerProvenance = "plugin_extras"

	// LayerOperatorDefaults is the operator's
	// `canonical_kinds_defaults` block — cross-kind operator
	// overrides applied to every active kind.
	LayerOperatorDefaults LayerProvenance = "operator_defaults"

	// LayerOperatorPerKind is the operator's per-kind block
	// (`canonical_kinds.<kind>`). Highest precedence; the
	// last-write-wins target on every gap field.
	LayerOperatorPerKind LayerProvenance = "operator"
)

// RegistryProvenance carries per-(kind, field) source-layer info
// for the merged canonical registry. The outer key is the kind
// name; the inner key is the gap field name; the value names the
// layer that last wrote that field (the layer that survived the
// last-write-wins merge).
//
// Instruction provenance is recorded at the key `"_instruction"`
// (reserved — the gap-field name shape forbids leading
// underscore so there's no collision with a real gap).
type RegistryProvenance map[string]map[string]LayerProvenance

// InstructionProvenanceKey is the reserved key under each kind's
// provenance map at which the instruction's source layer lives.
// Leading underscore forbidden in gap-field names guarantees no
// collision.
const InstructionProvenanceKey = "_instruction"

// BuiltinKindGapsList names every kind for which
// `BuiltinKindGaps` ships a non-empty default gap-set. Used by
// `/v1/canonical_registry/available` per #48 slice 3 to list
// Layer 1.5 kinds operators can discover + opt into. Order is
// stable + lexicographic for deterministic operator-facing
// output.
func BuiltinKindGapsList() []string {
	return []string{
		"article",
		"boardgame",
		"book",
		"person",
		"place",
		"recipe",
	}
}

// MergeCanonicalRegistryWithProvenance is `MergeCanonicalRegistry`
// with per-field source-layer tracking per #48 slice 3. The
// merged result is identical to the bare-return form; the second
// return adds a per-(kind, field) map naming the layer that
// supplied (or last overwrote) each spec — useful for the
// `/v1/canonical_registry/effective` operator-introspection
// surface.
//
// Provenance is recorded on every write into `gaps`. Subsequent
// overrides update the recorded source layer so the final value
// matches the surviving spec.
func MergeCanonicalRegistryWithProvenance(
	pluginGaps map[string]map[string]GapSpec,
	pluginEmittedKinds []string,
	opDefaults CanonicalKindConfig,
	opPerKind map[string]CanonicalKindConfig,
	logger *slog.Logger,
) (map[string]CanonicalKindConfig, RegistryProvenance) {
	// logger is reserved for the §5 plugin-vs-plugin conflict
	// WARN path which is deferred to a follow-up (needs per-
	// plugin field provenance). Kept on the signature so callers
	// don't need to touch the API again when that lands.
	_ = logger

	// Determine the kind set — union of plugin-emitted kinds and
	// operator-per-kind keys. Operator can pre-declare a kind
	// before any plugin emits it; that's the path for kinds not
	// yet supported by an active plugin but already configured.
	kinds := make(map[string]struct{})
	for _, k := range pluginEmittedKinds {
		kinds[k] = struct{}{}
	}
	for k := range opPerKind {
		kinds[k] = struct{}{}
	}

	out := make(map[string]CanonicalKindConfig, len(kinds))
	prov := make(RegistryProvenance, len(kinds))
	for kind := range kinds {
		// Layer 1: universal code defaults (every kind).
		gaps := DefaultGaps()
		gapProv := make(map[string]LayerProvenance, len(gaps))
		for fieldName := range gaps {
			gapProv[fieldName] = LayerUniversalDefaults
		}

		// Layer 1.5: kind-specific code defaults per ADR-0019 step 4
		// (e.g. boardgame's rating/owned/want/played operator gaps).
		// Layered before plugin extras so plugins can still override
		// any built-in that they want to reshape.
		for fieldName, spec := range BuiltinKindGaps(kind) {
			gaps[fieldName] = spec
			gapProv[fieldName] = LayerBuiltinKindGaps
		}

		// Layer 2: plugin extras for this kind.
		for fieldName, spec := range pluginGaps[kind] {
			gaps[fieldName] = spec
			gapProv[fieldName] = LayerPluginExtras
		}

		// Layer 3: operator-defaults (root). Adds / overrides
		// across every kind.
		for fieldName, spec := range opDefaults.Gaps {
			gaps[fieldName] = spec
			gapProv[fieldName] = LayerOperatorDefaults
		}

		// Layer 4: operator per-kind. Adds / overrides the
		// specific kind. Has the highest precedence.
		if perKind, ok := opPerKind[kind]; ok {
			for fieldName, spec := range perKind.Gaps {
				gaps[fieldName] = spec
				gapProv[fieldName] = LayerOperatorPerKind
			}
		}

		// Instruction: two-layer (operator-only). Always non-nil
		// in the merged result so handlers don't have to worry
		// about a nil-pointer dereference; the embedded value is
		// the code default (Enabled=false, Text="") when the
		// operator doesn't override.
		instr := DefaultInstruction()
		instrProv := LayerUniversalDefaults
		if opDefaults.Instruction != nil {
			instr = *opDefaults.Instruction
			instrProv = LayerOperatorDefaults
		}
		if perKind, ok := opPerKind[kind]; ok && perKind.Instruction != nil {
			instr = *perKind.Instruction
			instrProv = LayerOperatorPerKind
		}
		gapProv[InstructionProvenanceKey] = instrProv

		// ResolverPlugin per #276 — operator-only field; plugins
		// don't declare a resolver for their own emitted kinds
		// (the plugin IS the resolver when it emits canonical-
		// edge targets, so there's no separate field to set on
		// the plugin side). Pulled from opPerKind only.
		var resolverPlugin string
		if perKind, ok := opPerKind[kind]; ok {
			resolverPlugin = perKind.ResolverPlugin
		}

		out[kind] = CanonicalKindConfig{
			Gaps:           gaps,
			Instruction:    &instr,
			ResolverPlugin: resolverPlugin,
		}
		prov[kind] = gapProv
	}
	return out, prov
}
