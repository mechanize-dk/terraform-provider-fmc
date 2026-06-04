// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0
//
// This file is part of the mechanize-dk fork of terraform-provider-fmc and
// has no upstream counterpart. It implements the fmc_mze_manual_nat_rules
// resource — a bulk wrapper for /policy/ftdnatpolicies/{id}/manualnatrules
// with tenant-scoped (match_on) cooperative ownership.
//
// See docs/superpowers/specs/2026-06-04-bulk-nat-tenant-resources-design.md
// for the design rationale.

package provider

import (
	"context"
	"strings"

	"github.com/CiscoDevNet/terraform-provider-fmc/internal/provider/helpers"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/netascode/go-fmc"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ─── Model types ─────────────────────────────────────────────────────────────

type MzeManualNatRules struct {
	ID             types.String              `tfsdk:"id"`
	Domain         types.String              `tfsdk:"domain"`
	FtdNatPolicyID types.String              `tfsdk:"ftd_nat_policy_id"`
	MatchOn        *MzeManualNatRulesMatchOn `tfsdk:"match_on"`
	BeforeAuto     []MzeManualNatRulesItem   `tfsdk:"before_auto"`
	AfterAuto      []MzeManualNatRulesItem   `tfsdk:"after_auto"`
	// before_auto_by_key / after_auto_by_key (computed map views) are deferred
	// to v1.1 — TF Framework's nested-Map Computed-without-state model needs
	// a typed wrapper to avoid "received unknown value" conversion errors.
}

type MzeManualNatRulesMatchOn struct {
	SourceInterfaceID      types.String `tfsdk:"source_interface_id"`
	DestinationInterfaceID types.String `tfsdk:"destination_interface_id"`
}

type MzeManualNatRulesItem struct {
	Key                            types.String `tfsdk:"key"`
	ID                             types.String `tfsdk:"id"`
	Description                    types.String `tfsdk:"description"`
	Enabled                        types.Bool   `tfsdk:"enabled"`
	NatType                        types.String `tfsdk:"nat_type"`
	FallThrough                    types.Bool   `tfsdk:"fall_through"`
	InterfaceInOriginalDestination types.Bool   `tfsdk:"interface_in_original_destination"`
	InterfaceInTranslatedSource    types.Bool   `tfsdk:"interface_in_translated_source"`
	IPv6                           types.Bool   `tfsdk:"ipv6"`
	NetToNet                       types.Bool   `tfsdk:"net_to_net"`
	NoProxyArp                     types.Bool   `tfsdk:"no_proxy_arp"`
	Unidirectional                 types.Bool   `tfsdk:"unidirectional"`
	SourceInterfaceID              types.String `tfsdk:"source_interface_id"`
	DestinationInterfaceID         types.String `tfsdk:"destination_interface_id"`
	OriginalSourceID               types.String `tfsdk:"original_source_id"`
	OriginalDestinationID          types.String `tfsdk:"original_destination_id"`
	OriginalSourcePortID           types.String `tfsdk:"original_source_port_id"`
	OriginalDestinationPortID      types.String `tfsdk:"original_destination_port_id"`
	TranslatedSourceID             types.String `tfsdk:"translated_source_id"`
	TranslatedDestinationID        types.String `tfsdk:"translated_destination_id"`
	TranslatedSourcePortID         types.String `tfsdk:"translated_source_port_id"`
	TranslatedDestinationPortID    types.String `tfsdk:"translated_destination_port_id"`
}

// ─── Resource type + framework boilerplate ───────────────────────────────────

var (
	_ resource.Resource                = &MzeManualNatRulesResource{}
	_ resource.ResourceWithImportState = &MzeManualNatRulesResource{}
)

func NewMzeManualNatRulesResource() resource.Resource {
	return &MzeManualNatRulesResource{}
}

type MzeManualNatRulesResource struct {
	client *fmc.Client
}

func (r *MzeManualNatRulesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mze_manual_nat_rules"
}

func (r *MzeManualNatRulesResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = req.ProviderData.(*FmcProviderData).Client
}

// itemSchema returns the per-item nested schema, shared between before_auto and after_auto.
func mzeManualNatRulesItemSchema() schema.NestedAttributeObject {
	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"key": schema.StringAttribute{
				MarkdownDescription: helpers.NewAttributeDescription("Tenant-chosen identifier for this rule. Used by the resource to track item identity across plans; never sent to FMC.").String,
				Required:            true,
			},
			"id": schema.StringAttribute{
				MarkdownDescription: helpers.NewAttributeDescription("FMC rule UUID (computed after POST).").String,
				Computed:            true,
				// UseStateForUnknown deliberately omitted: when a new item is
				// added to an existing list, the framework marks per-item
				// Computed fields with UseStateForUnknown as Null at
				// plan-time (not Unknown), which then collides with the
				// post-apply id and surfaces as "inconsistent result".
			},
			"description":                       schema.StringAttribute{Optional: true},
			"enabled":                           schema.BoolAttribute{Optional: true},
			"nat_type":                          schema.StringAttribute{Required: true, MarkdownDescription: "STATIC or DYNAMIC"},
			"fall_through":                      schema.BoolAttribute{Optional: true},
			"interface_in_original_destination": schema.BoolAttribute{Optional: true},
			"interface_in_translated_source":    schema.BoolAttribute{Optional: true},
			"ipv6":                              schema.BoolAttribute{Optional: true},
			"net_to_net":                        schema.BoolAttribute{Optional: true},
			"no_proxy_arp":                      schema.BoolAttribute{Optional: true},
			"unidirectional":                    schema.BoolAttribute{Optional: true},
			"source_interface_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Auto-filled from match_on when omitted.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"destination_interface_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Auto-filled from match_on when omitted.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"original_source_id":             schema.StringAttribute{Optional: true},
			"original_destination_id":        schema.StringAttribute{Optional: true},
			"original_source_port_id":        schema.StringAttribute{Optional: true},
			"original_destination_port_id":   schema.StringAttribute{Optional: true},
			"translated_source_id":           schema.StringAttribute{Optional: true},
			"translated_destination_id":      schema.StringAttribute{Optional: true},
			"translated_source_port_id":      schema.StringAttribute{Optional: true},
			"translated_destination_port_id": schema.StringAttribute{Optional: true},
		},
	}
}

func (r *MzeManualNatRulesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: helpers.NewAttributeDescription(
			"Bulk-manages a tenant-scoped set of FTD manual NAT rules within a shared NAT policy. " +
				"Use `before_auto` and `after_auto` (ordered lists, each item carries a tenant-chosen `key`) to declare rules; the resource POSTs them in list order. " +
				"`match_on` declares the tenant scope and auto-fills the matching fields onto items that omit them. " +
				"Cooperative ownership — other tenants' rules in the same section are ignored.").String,

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Synthetic id: <ftd_nat_policy_id>:<sha256(match_on)[:16]>",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"domain": schema.StringAttribute{
				Optional:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"ftd_nat_policy_id": schema.StringAttribute{
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"match_on": schema.SingleNestedAttribute{
				Required:      true,
				PlanModifiers: []planmodifier.Object{&matchOnRequiresReplace{}},
				Attributes: map[string]schema.Attribute{
					"source_interface_id":      schema.StringAttribute{Optional: true},
					"destination_interface_id": schema.StringAttribute{Optional: true},
				},
			},
			"before_auto": schema.ListNestedAttribute{
				Optional:     true,
				NestedObject: mzeManualNatRulesItemSchema(),
			},
			"after_auto": schema.ListNestedAttribute{
				Optional:     true,
				NestedObject: mzeManualNatRulesItemSchema(),
			},
		},
	}
}

// matchOnRequiresReplace implements ObjectPlanModifier — any change to
// match_on triggers replace of the resource.
type matchOnRequiresReplace struct{}

func (m *matchOnRequiresReplace) Description(_ context.Context) string {
	return "Changing match_on replaces the resource."
}
func (m *matchOnRequiresReplace) MarkdownDescription(_ context.Context) string {
	return m.Description(nil)
}
func (m *matchOnRequiresReplace) PlanModifyObject(_ context.Context, req planmodifier.ObjectRequest, resp *planmodifier.ObjectResponse) {
	if req.StateValue.IsNull() {
		return
	}
	if !req.PlanValue.Equal(req.StateValue) {
		resp.RequiresReplace = true
	}
}

// ─── Private helpers ─────────────────────────────────────────────────────────

// extract pulls a helpers.MatchOn out of the framework model.
func (m *MzeManualNatRulesMatchOn) extract() helpers.MatchOn {
	if m == nil {
		return helpers.MatchOn{}
	}
	return helpers.MatchOn{
		SourceInterfaceID:      m.SourceInterfaceID.ValueString(),
		DestinationInterfaceID: m.DestinationInterfaceID.ValueString(),
	}
}

// syntheticID returns "<ftd_nat_policy_id>:<match_on_hash>".
func (r *MzeManualNatRules) syntheticID() string {
	return r.FtdNatPolicyID.ValueString() + ":" + r.MatchOn.extract().Hash()
}

// resourcePath returns the FMC URL for the manualnatrules collection on this resource's policy.
func (r *MzeManualNatRules) resourcePath() string {
	return "/api/fmc_config/v1/domain/{DOMAIN_UUID}/policy/ftdnatpolicies/" + r.FtdNatPolicyID.ValueString() + "/manualnatrules"
}

// fieldsForMatchOn returns the item's fields as the map[string]*string shape
// that helpers.MatchOn.Validate and AutoFill expect.
//
// nil pointer = explicitly unset; missing key = unknown / to-be-filled.
func (it MzeManualNatRulesItem) fieldsForMatchOn() map[string]*string {
	out := map[string]*string{}
	put := func(name string, v types.String) {
		if v.IsUnknown() {
			return
		}
		if v.IsNull() {
			out[name] = nil
			return
		}
		s := v.ValueString()
		out[name] = &s
	}
	put("source_interface_id", it.SourceInterfaceID)
	put("destination_interface_id", it.DestinationInterfaceID)
	return out
}

// applyAutoFill mutates `it` so that any matcher-declared field the item
// omitted gets injected, mirroring the schema's Computed semantics so the
// post-Create state value isn't unknown. Any Optional+Computed field that
// the matcher didn't fill AND the item left Unknown is collapsed to Null,
// because the framework rejects Unknown values in the post-apply state.
func (it *MzeManualNatRulesItem) applyAutoFill(matcher helpers.MatchOn) {
	fields := it.fieldsForMatchOn()
	matcher.AutoFill(fields)
	if v, ok := fields["source_interface_id"]; ok && v != nil && (it.SourceInterfaceID.IsNull() || it.SourceInterfaceID.IsUnknown()) {
		it.SourceInterfaceID = types.StringValue(*v)
	} else if it.SourceInterfaceID.IsUnknown() {
		it.SourceInterfaceID = types.StringNull()
	}
	if v, ok := fields["destination_interface_id"]; ok && v != nil && (it.DestinationInterfaceID.IsNull() || it.DestinationInterfaceID.IsUnknown()) {
		it.DestinationInterfaceID = types.StringValue(*v)
	} else if it.DestinationInterfaceID.IsUnknown() {
		it.DestinationInterfaceID = types.StringNull()
	}
}

// toBody builds the FMC POST/PUT body for a single rule. The `section`
// argument is currently only used as a key for the diffByKey body comparison
// ("X" placeholder) — the actual section assignment is via the ?section=
// URL query parameter, not the body. (FMC rejects a `section` body field as
// "Unrecognized" — verified against the lab on 2026-06-04.)
func (it MzeManualNatRulesItem) toBody(section string) string {
	_ = section // body-side section field would be rejected; URL ?section= is canonical
	body := `{"type":"FTDManualNatRule"}`
	body, _ = sjson.Set(body, "natType", it.NatType.ValueString())
	if !it.Description.IsNull() && !it.Description.IsUnknown() {
		body, _ = sjson.Set(body, "description", it.Description.ValueString())
	}
	if !it.Enabled.IsNull() && !it.Enabled.IsUnknown() {
		body, _ = sjson.Set(body, "enabled", it.Enabled.ValueBool())
	}
	for _, b := range []struct {
		path string
		v    types.Bool
	}{
		{"fallThrough", it.FallThrough},
		{"interfaceInOriginalDestination", it.InterfaceInOriginalDestination},
		{"interfaceInTranslatedSource", it.InterfaceInTranslatedSource},
		{"interfaceIpv6", it.IPv6},
		{"netToNet", it.NetToNet},
		{"noProxyArp", it.NoProxyArp},
		{"unidirectional", it.Unidirectional},
	} {
		if !b.v.IsNull() && !b.v.IsUnknown() {
			body, _ = sjson.Set(body, b.path, b.v.ValueBool())
		}
	}
	for _, r := range []struct {
		path string
		id   types.String
	}{
		{"sourceInterface.id", it.SourceInterfaceID},
		{"destinationInterface.id", it.DestinationInterfaceID},
		{"originalSource.id", it.OriginalSourceID},
		{"originalDestination.id", it.OriginalDestinationID},
		{"originalSourcePort.id", it.OriginalSourcePortID},
		{"originalDestinationPort.id", it.OriginalDestinationPortID},
		{"translatedSource.id", it.TranslatedSourceID},
		{"translatedDestination.id", it.TranslatedDestinationID},
		{"translatedSourcePort.id", it.TranslatedSourcePortID},
		{"translatedDestinationPort.id", it.TranslatedDestinationPortID},
	} {
		if !r.id.IsNull() && !r.id.IsUnknown() && r.id.ValueString() != "" {
			body, _ = sjson.Set(body, r.path, r.id.ValueString())
		}
	}
	return body
}

// fromBody populates an item from an FMC GET response body.
func (it *MzeManualNatRulesItem) fromBody(res gjson.Result) {
	it.ID = types.StringValue(res.Get("id").String())
	if d := res.Get("description"); d.Exists() {
		it.Description = types.StringValue(d.String())
	}
	if e := res.Get("enabled"); e.Exists() {
		// Only adopt FMC's value if state was already non-Null (drift refresh)
		// or Unknown. A user-omitted Optional bool reads as Null; writing
		// FMC's default into state would produce a "provider produced
		// inconsistent result after apply" error.
		if it.Enabled.IsUnknown() {
			it.Enabled = types.BoolValue(e.Bool())
		} else if !it.Enabled.IsNull() {
			it.Enabled = types.BoolValue(e.Bool())
		}
	}
	if v := res.Get("natType"); v.Exists() {
		it.NatType = types.StringValue(v.String())
	}
	// fromBody for Optional-only bool fields: only adopt FMC's value if state
	// was already non-Null (drift refresh) or Unknown. A user-omitted
	// Optional bool reads as Null; writing FMC's default into state would
	// produce a "provider produced inconsistent result after apply" error.
	for _, b := range []struct {
		path string
		dst  *types.Bool
	}{
		{"fallThrough", &it.FallThrough},
		{"interfaceInOriginalDestination", &it.InterfaceInOriginalDestination},
		{"interfaceInTranslatedSource", &it.InterfaceInTranslatedSource},
		{"interfaceIpv6", &it.IPv6},
		{"netToNet", &it.NetToNet},
		{"noProxyArp", &it.NoProxyArp},
		{"unidirectional", &it.Unidirectional},
	} {
		if v := res.Get(b.path); v.Exists() {
			if b.dst.IsUnknown() || !b.dst.IsNull() {
				*b.dst = types.BoolValue(v.Bool())
			}
		}
	}
	for _, r := range []struct {
		path string
		dst  *types.String
	}{
		{"sourceInterface.id", &it.SourceInterfaceID},
		{"destinationInterface.id", &it.DestinationInterfaceID},
		{"originalSource.id", &it.OriginalSourceID},
		{"originalDestination.id", &it.OriginalDestinationID},
		{"originalSourcePort.id", &it.OriginalSourcePortID},
		{"originalDestinationPort.id", &it.OriginalDestinationPortID},
		{"translatedSource.id", &it.TranslatedSourceID},
		{"translatedDestination.id", &it.TranslatedDestinationID},
		{"translatedSourcePort.id", &it.TranslatedSourcePortID},
		{"translatedDestinationPort.id", &it.TranslatedDestinationPortID},
	} {
		if v := res.Get(r.path); v.Exists() {
			*r.dst = types.StringValue(v.String())
		}
	}
}

// ─── CRUD stubs — filled in by later tasks ───────────────────────────────────

func (r *MzeManualNatRulesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan MzeManualNatRules
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	matcher := plan.MatchOn.extract()
	if matcher.IsEmpty() {
		resp.Diagnostics.AddError("invalid match_on", "match_on must declare at least one field")
		return
	}

	postSection := func(section string, items []MzeManualNatRulesItem) ([]MzeManualNatRulesItem, bool) {
		for i := range items {
			it := items[i]
			if err := matcher.Validate(it.Key.ValueString(), it.fieldsForMatchOn()); err != nil {
				resp.Diagnostics.AddError("match_on validation failed", err.Error())
				return items, false
			}
			it.applyAutoFill(matcher)
			path := plan.resourcePath() + "?section=" + strings.ToLower(section)
			body := it.toBody(section)
			postRes, postErr := r.client.Post(path, body, fmc.DomainName(plan.Domain.ValueString()))
			if postErr != nil {
				rec, recErr := helpers.FindDuplicateByIndex(ctx, r.client, plan.resourcePath(), postErr, postRes,
					fmc.DomainName(plan.Domain.ValueString()))
				if recErr != nil {
					resp.Diagnostics.AddError("manual NAT POST failed for key "+it.Key.ValueString(),
						"err="+recErr.Error()+"; body="+body+"; resp="+postRes.String())
					return items, false
				}
				it.fromBody(rec)
			} else {
				it.fromBody(postRes)
			}
			items[i] = it
		}
		return items, true
	}

	var ok bool
	plan.BeforeAuto, ok = postSection("BEFORE_AUTO", plan.BeforeAuto)
	if !ok {
		return
	}
	plan.AfterAuto, ok = postSection("AFTER_AUTO", plan.AfterAuto)
	if !ok {
		return
	}

	plan.ID = types.StringValue(plan.syntheticID())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *MzeManualNatRulesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state MzeManualNatRules
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	refresh := func(items []MzeManualNatRulesItem) []MzeManualNatRulesItem {
		// Preserve nil-vs-empty distinction across refresh. Returning a
		// fresh `make([]T, 0, …)` for a nil input would surface "null -> []"
		// drift on the next plan when the user omitted the field.
		if items == nil {
			return nil
		}
		out := make([]MzeManualNatRulesItem, 0, len(items))
		for _, it := range items {
			if it.ID.IsNull() || it.ID.IsUnknown() || it.ID.ValueString() == "" {
				continue
			}
			res, err := r.client.Get(state.resourcePath()+"/"+it.ID.ValueString(),
				fmc.DomainName(state.Domain.ValueString()))
			if err != nil {
				if strings.Contains(err.Error(), "StatusCode 404") {
					tflog.Debug(ctx, "manual NAT rule "+it.ID.ValueString()+" gone on FMC, dropping from state")
					continue
				}
				resp.Diagnostics.AddError("FMC GET failed", err.Error())
				return items
			}
			it.fromBody(res)
			out = append(out, it)
		}
		return out
	}

	state.BeforeAuto = refresh(state.BeforeAuto)
	state.AfterAuto = refresh(state.AfterAuto)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// mzeManualNatRulesDiffEntry classifies one key during Update.
type mzeManualNatRulesDiffEntry struct {
	key      string
	statePos int // -1 if absent in state
	planPos  int // -1 if absent in plan
	stateID  string
}

// diffByKey computes the bulk-resource diff classes for one section.
// Returns: removed, modified (body changed, same position), reordered
// (position changed, body may or may not have changed), added.
func mzeManualNatRulesDiffByKey(state, plan []MzeManualNatRulesItem) (removed, modified, reordered, added []mzeManualNatRulesDiffEntry) {
	byKey := map[string]*mzeManualNatRulesDiffEntry{}
	for i := range state {
		k := state[i].Key.ValueString()
		byKey[k] = &mzeManualNatRulesDiffEntry{key: k, statePos: i, planPos: -1, stateID: state[i].ID.ValueString()}
	}
	for i := range plan {
		k := plan[i].Key.ValueString()
		if e, ok := byKey[k]; ok {
			e.planPos = i
		} else {
			byKey[k] = &mzeManualNatRulesDiffEntry{key: k, statePos: -1, planPos: i}
		}
	}
	for _, e := range byKey {
		switch {
		case e.statePos == -1:
			added = append(added, *e)
		case e.planPos == -1:
			removed = append(removed, *e)
		default:
			bodyChanged := state[e.statePos].toBody("X") != plan[e.planPos].toBody("X")
			posChanged := e.statePos != e.planPos
			switch {
			case posChanged:
				reordered = append(reordered, *e)
			case bodyChanged:
				modified = append(modified, *e)
			}
		}
	}
	return
}

func (r *MzeManualNatRulesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state MzeManualNatRules
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	matcher := plan.MatchOn.extract()

	// Compute diffs for both sections up front, then execute in cross-section
	// stages: all DELETEs → all PUTs → all POSTs. Cross-section moves (rule
	// migrates BEFORE_AUTO ↔ AFTER_AUTO) would otherwise hit a duplicate POST
	// before the matching DELETE runs.

	type sectionPlan struct {
		section      string
		statePos     []MzeManualNatRulesItem
		planPos      []MzeManualNatRulesItem
		removed      []mzeManualNatRulesDiffEntry
		modified     []mzeManualNatRulesDiffEntry
		reordered    []mzeManualNatRulesDiffEntry
		added        []mzeManualNatRulesDiffEntry
		stateIDByKey map[string]string
		needsPOST    map[string]bool
		isModified   map[string]bool
	}

	buildPlan := func(section string, stateItems, planItems []MzeManualNatRulesItem) sectionPlan {
		rem, mod, reord, add := mzeManualNatRulesDiffByKey(stateItems, planItems)
		sp := sectionPlan{
			section:      section,
			statePos:     stateItems,
			planPos:      planItems,
			removed:      rem,
			modified:     mod,
			reordered:    reord,
			added:        add,
			stateIDByKey: map[string]string{},
			needsPOST:    map[string]bool{},
			isModified:   map[string]bool{},
		}
		for _, it := range stateItems {
			sp.stateIDByKey[it.Key.ValueString()] = it.ID.ValueString()
		}
		for _, e := range add {
			sp.needsPOST[e.key] = true
		}
		for _, e := range reord {
			sp.needsPOST[e.key] = true
		}
		for _, e := range mod {
			sp.isModified[e.key] = true
		}
		return sp
	}

	sps := []sectionPlan{
		buildPlan("BEFORE_AUTO", state.BeforeAuto, plan.BeforeAuto),
		buildPlan("AFTER_AUTO", state.AfterAuto, plan.AfterAuto),
	}

	// ── Phase 1 — DELETE removed + reordered (across both sections). ─────────
	for i := range sps {
		sp := &sps[i]
		deleteSet := map[string]bool{}
		for _, e := range sp.removed {
			deleteSet[e.key] = true
		}
		for _, e := range sp.reordered {
			deleteSet[e.key] = true
		}
		for k := range deleteSet {
			id := sp.stateIDByKey[k]
			if id == "" {
				continue
			}
			_, err := r.client.Delete(plan.resourcePath()+"/"+id,
				fmc.DomainName(plan.Domain.ValueString()))
			if err != nil && !strings.Contains(err.Error(), "StatusCode 404") {
				resp.Diagnostics.AddError("DELETE failed for key "+k+" (section "+sp.section+")", err.Error())
				return
			}
		}
	}

	// ── Phase 2 — PUT modified ∧ ¬reordered. ────────────────────────────────
	for i := range sps {
		sp := &sps[i]
		for _, e := range sp.modified {
			it := sp.planPos[e.planPos]
			if err := matcher.Validate(it.Key.ValueString(), it.fieldsForMatchOn()); err != nil {
				resp.Diagnostics.AddError("match_on validation failed", err.Error())
				return
			}
			it.applyAutoFill(matcher)
			id := sp.stateIDByKey[e.key]
			body := it.toBody(sp.section)
			body, _ = sjson.Set(body, "id", id)
			res, err := r.client.Put(plan.resourcePath()+"/"+id, body,
				fmc.DomainName(plan.Domain.ValueString()))
			if err != nil {
				resp.Diagnostics.AddError("PUT failed for key "+it.Key.ValueString(), err.Error())
				return
			}
			it.fromBody(res)
			sp.planPos[e.planPos] = it
		}
	}

	// ── Phase 3 — POST added + reordered (in list order, across sections). ──
	for i := range sps {
		sp := &sps[i]
		for j := range sp.planPos {
			k := sp.planPos[j].Key.ValueString()
			if sp.needsPOST[k] {
				it := sp.planPos[j]
				if err := matcher.Validate(it.Key.ValueString(), it.fieldsForMatchOn()); err != nil {
					resp.Diagnostics.AddError("match_on validation failed", err.Error())
					return
				}
				it.applyAutoFill(matcher)
				path := plan.resourcePath() + "?section=" + strings.ToLower(sp.section)
				body := it.toBody(sp.section)
				postRes, postErr := r.client.Post(path, body, fmc.DomainName(plan.Domain.ValueString()))
				if postErr != nil {
					rec, recErr := helpers.FindDuplicateByIndex(ctx, r.client, plan.resourcePath(), postErr, postRes,
						fmc.DomainName(plan.Domain.ValueString()))
					if recErr != nil {
						resp.Diagnostics.AddError("POST failed for key "+it.Key.ValueString(), recErr.Error())
						return
					}
					it.fromBody(rec)
				} else {
					it.fromBody(postRes)
				}
				sp.planPos[j] = it
				continue
			}
			if sp.isModified[k] {
				continue
			}
			// Unchanged — preserve stored ID.
			if sp.planPos[j].ID.IsNull() || sp.planPos[j].ID.IsUnknown() {
				if id, ok := sp.stateIDByKey[k]; ok && id != "" {
					sp.planPos[j].ID = types.StringValue(id)
				}
			}
		}
	}

	plan.BeforeAuto = sps[0].planPos
	plan.AfterAuto = sps[1].planPos

	plan.ID = types.StringValue(plan.syntheticID())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *MzeManualNatRulesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state MzeManualNatRules
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	deleteAll := func(items []MzeManualNatRulesItem) bool {
		for _, it := range items {
			if it.ID.IsNull() || it.ID.ValueString() == "" {
				continue
			}
			_, err := r.client.Delete(state.resourcePath()+"/"+it.ID.ValueString(),
				fmc.DomainName(state.Domain.ValueString()))
			if err != nil && !strings.Contains(err.Error(), "StatusCode 404") {
				resp.Diagnostics.AddError("DELETE failed", err.Error())
				return false
			}
		}
		return true
	}
	if !deleteAll(state.BeforeAuto) {
		return
	}
	if !deleteAll(state.AfterAuto) {
		return
	}
}

func (r *MzeManualNatRulesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, ":", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError("invalid import id",
			"expected <ftd_nat_policy_id>:<match_on_hash>, got "+req.ID)
		return
	}
	// match_on and the rule lists are populated by the next Read after the
	// user supplies match_on in HCL.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("ftd_nat_policy_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
