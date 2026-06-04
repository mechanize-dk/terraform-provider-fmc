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
	ID              types.String                                 `tfsdk:"id"`
	Domain          types.String                                 `tfsdk:"domain"`
	FtdNatPolicyID  types.String                                 `tfsdk:"ftd_nat_policy_id"`
	MatchOn         *MzeManualNatRulesMatchOn                    `tfsdk:"match_on"`
	BeforeAuto      []MzeManualNatRulesItem                      `tfsdk:"before_auto"`
	AfterAuto       []MzeManualNatRulesItem                      `tfsdk:"after_auto"`
	BeforeAutoByKey map[string]MzeManualNatRulesItem             `tfsdk:"before_auto_by_key"`
	AfterAutoByKey  map[string]MzeManualNatRulesItem             `tfsdk:"after_auto_by_key"`
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
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
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
			"before_auto_by_key": schema.MapNestedAttribute{
				MarkdownDescription: "Derived map view of `before_auto`, keyed by each item's `key`. Read-only; populated by the resource for downstream reference.",
				Computed:            true,
				NestedObject:        mzeManualNatRulesItemSchema(),
			},
			"after_auto_by_key": schema.MapNestedAttribute{
				MarkdownDescription: "Derived map view of `after_auto`, keyed by each item's `key`. Read-only.",
				Computed:            true,
				NestedObject:        mzeManualNatRulesItemSchema(),
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
// post-Create state value isn't unknown.
func (it *MzeManualNatRulesItem) applyAutoFill(matcher helpers.MatchOn) {
	fields := it.fieldsForMatchOn()
	matcher.AutoFill(fields)
	if v, ok := fields["source_interface_id"]; ok && v != nil && (it.SourceInterfaceID.IsNull() || it.SourceInterfaceID.IsUnknown()) {
		it.SourceInterfaceID = types.StringValue(*v)
	}
	if v, ok := fields["destination_interface_id"]; ok && v != nil && (it.DestinationInterfaceID.IsNull() || it.DestinationInterfaceID.IsUnknown()) {
		it.DestinationInterfaceID = types.StringValue(*v)
	}
}

// toBody builds the FMC POST/PUT body for a single rule. section must be
// "BEFORE_AUTO" or "AFTER_AUTO". The body excludes the tenant `key` (TF-only).
func (it MzeManualNatRulesItem) toBody(section string) string {
	body := `{"type":"FTDManualNatRule"}`
	body, _ = sjson.Set(body, "section", section)
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
		it.Enabled = types.BoolValue(e.Bool())
	}
	if v := res.Get("natType"); v.Exists() {
		it.NatType = types.StringValue(v.String())
	}
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
			*b.dst = types.BoolValue(v.Bool())
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

// rebuildByKey populates the *_by_key maps from the ordered lists. Idempotent.
func (r *MzeManualNatRules) rebuildByKey() {
	r.BeforeAutoByKey = map[string]MzeManualNatRulesItem{}
	for _, it := range r.BeforeAuto {
		r.BeforeAutoByKey[it.Key.ValueString()] = it
	}
	r.AfterAutoByKey = map[string]MzeManualNatRulesItem{}
	for _, it := range r.AfterAuto {
		r.AfterAutoByKey[it.Key.ValueString()] = it
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
					resp.Diagnostics.AddError("manual NAT POST failed for key "+it.Key.ValueString(), recErr.Error())
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
	plan.rebuildByKey()
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *MzeManualNatRulesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state MzeManualNatRules
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	refresh := func(items []MzeManualNatRulesItem) []MzeManualNatRulesItem {
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
	state.rebuildByKey()

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// mzeManualNatRulesDiffEntry classifies one key during Update.
type mzeManualNatRulesDiffEntry struct {
	key      string
	statePos int  // -1 if absent in state
	planPos  int  // -1 if absent in plan
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

	updateSection := func(section string, stateItems, planItems []MzeManualNatRulesItem) ([]MzeManualNatRulesItem, bool) {
		removed, modified, reordered, added := mzeManualNatRulesDiffByKey(stateItems, planItems)

		// state ID lookup
		stateIDByKey := map[string]string{}
		for _, it := range stateItems {
			stateIDByKey[it.Key.ValueString()] = it.ID.ValueString()
		}

		// classification sets
		needsPOST := map[string]bool{}
		for _, e := range added {
			needsPOST[e.key] = true
		}
		for _, e := range reordered {
			needsPOST[e.key] = true
		}
		isModified := map[string]bool{}
		for _, e := range modified {
			isModified[e.key] = true
		}

		// Phase 1 — DELETE removed + reordered.
		deleteSet := map[string]bool{}
		for _, e := range removed {
			deleteSet[e.key] = true
		}
		for _, e := range reordered {
			deleteSet[e.key] = true
		}
		for k := range deleteSet {
			id := stateIDByKey[k]
			if id == "" {
				continue
			}
			_, err := r.client.Delete(plan.resourcePath()+"/"+id,
				fmc.DomainName(plan.Domain.ValueString()))
			if err != nil && !strings.Contains(err.Error(), "StatusCode 404") {
				resp.Diagnostics.AddError("DELETE failed for key "+k, err.Error())
				return planItems, false
			}
		}

		// Phase 2 — PUT modified ∧ ¬reordered.
		for _, e := range modified {
			it := planItems[e.planPos]
			if err := matcher.Validate(it.Key.ValueString(), it.fieldsForMatchOn()); err != nil {
				resp.Diagnostics.AddError("match_on validation failed", err.Error())
				return planItems, false
			}
			it.applyAutoFill(matcher)
			id := stateIDByKey[e.key]
			body := it.toBody(section)
			body, _ = sjson.Set(body, "id", id)
			res, err := r.client.Put(plan.resourcePath()+"/"+id, body,
				fmc.DomainName(plan.Domain.ValueString()))
			if err != nil {
				resp.Diagnostics.AddError("PUT failed for key "+it.Key.ValueString(), err.Error())
				return planItems, false
			}
			it.fromBody(res)
			planItems[e.planPos] = it
		}

		// Phase 3 — walk planItems in order. POST what needs POST; otherwise
		// preserve the stored ID for unchanged items (modified items already
		// have their refreshed body from the PUT).
		for i := range planItems {
			k := planItems[i].Key.ValueString()
			if needsPOST[k] {
				it := planItems[i]
				if err := matcher.Validate(it.Key.ValueString(), it.fieldsForMatchOn()); err != nil {
					resp.Diagnostics.AddError("match_on validation failed", err.Error())
					return planItems, false
				}
				it.applyAutoFill(matcher)
				path := plan.resourcePath() + "?section=" + strings.ToLower(section)
				body := it.toBody(section)
				postRes, postErr := r.client.Post(path, body, fmc.DomainName(plan.Domain.ValueString()))
				if postErr != nil {
					rec, recErr := helpers.FindDuplicateByIndex(ctx, r.client, plan.resourcePath(), postErr, postRes,
						fmc.DomainName(plan.Domain.ValueString()))
					if recErr != nil {
						resp.Diagnostics.AddError("POST failed for key "+it.Key.ValueString(), recErr.Error())
						return planItems, false
					}
					it.fromBody(rec)
				} else {
					it.fromBody(postRes)
				}
				planItems[i] = it
				continue
			}
			if isModified[k] {
				// Already handled in Phase 2.
				continue
			}
			// Unchanged — preserve stored ID.
			if planItems[i].ID.IsNull() || planItems[i].ID.IsUnknown() {
				if id, ok := stateIDByKey[k]; ok && id != "" {
					planItems[i].ID = types.StringValue(id)
				}
			}
		}
		return planItems, true
	}

	var ok bool
	plan.BeforeAuto, ok = updateSection("BEFORE_AUTO", state.BeforeAuto, plan.BeforeAuto)
	if !ok {
		return
	}
	plan.AfterAuto, ok = updateSection("AFTER_AUTO", state.AfterAuto, plan.AfterAuto)
	if !ok {
		return
	}

	plan.ID = types.StringValue(plan.syntheticID())
	plan.rebuildByKey()
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *MzeManualNatRulesResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *MzeManualNatRulesResource) ImportState(_ context.Context, _ resource.ImportStateRequest, _ *resource.ImportStateResponse) {
}
