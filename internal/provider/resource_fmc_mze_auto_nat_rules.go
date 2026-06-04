// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0
//
// This file is part of the mechanize-dk fork of terraform-provider-fmc and
// has no upstream counterpart. It implements the fmc_mze_auto_nat_rules
// resource — a bulk wrapper for /policy/ftdnatpolicies/{id}/autonatrules
// with tenant-scoped (match_on) cooperative ownership.
//
// Auto-NAT rules are unordered (FMC orders them by specificity at runtime).
// The Terraform map is the source of truth for membership; rule ID is the
// stable refresh anchor.
//
// See docs/superpowers/specs/2026-06-04-bulk-nat-tenant-resources-design.md.

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

type MzeAutoNatRules struct {
	ID             types.String                   `tfsdk:"id"`
	Domain         types.String                   `tfsdk:"domain"`
	FtdNatPolicyID types.String                   `tfsdk:"ftd_nat_policy_id"`
	MatchOn        *MzeManualNatRulesMatchOn      `tfsdk:"match_on"` // identical shape — reuse the type
	Rules          map[string]MzeAutoNatRulesItem `tfsdk:"rules"`
}

type MzeAutoNatRulesItem struct {
	ID                                      types.String `tfsdk:"id"`
	NatType                                 types.String `tfsdk:"nat_type"`
	OriginalNetworkID                       types.String `tfsdk:"original_network_id"`
	TranslatedNetworkID                     types.String `tfsdk:"translated_network_id"`
	SourceInterfaceID                       types.String `tfsdk:"source_interface_id"`
	DestinationInterfaceID                  types.String `tfsdk:"destination_interface_id"`
	OriginalPort                            types.Int64  `tfsdk:"original_port"`
	TranslatedPort                          types.Int64  `tfsdk:"translated_port"`
	Protocol                                types.String `tfsdk:"protocol"`
	FallThrough                             types.Bool   `tfsdk:"fall_through"`
	IPv6                                    types.Bool   `tfsdk:"ipv6"`
	NetToNet                                types.Bool   `tfsdk:"net_to_net"`
	NoProxyArp                              types.Bool   `tfsdk:"no_proxy_arp"`
	PerformRouteLookup                      types.Bool   `tfsdk:"perform_route_lookup"`
	TranslateDNS                            types.Bool   `tfsdk:"translate_dns"`
	TranslatedNetworkIsDestinationInterface types.Bool   `tfsdk:"translated_network_is_destination_interface"`
}

// ─── Resource type + framework boilerplate ───────────────────────────────────

var (
	_ resource.Resource                = &MzeAutoNatRulesResource{}
	_ resource.ResourceWithImportState = &MzeAutoNatRulesResource{}
)

func NewMzeAutoNatRulesResource() resource.Resource { return &MzeAutoNatRulesResource{} }

type MzeAutoNatRulesResource struct{ client *fmc.Client }

func (r *MzeAutoNatRulesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mze_auto_nat_rules"
}

func (r *MzeAutoNatRulesResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = req.ProviderData.(*FmcProviderData).Client
}

func (r *MzeAutoNatRulesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	autoItem := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"id":                                          schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		"nat_type":                                    schema.StringAttribute{Required: true, MarkdownDescription: "STATIC or DYNAMIC"},
		"original_network_id":                         schema.StringAttribute{Required: true},
		"translated_network_id":                       schema.StringAttribute{Optional: true},
		"source_interface_id":                         schema.StringAttribute{Optional: true, Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		"destination_interface_id":                    schema.StringAttribute{Optional: true, Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		"original_port":                               schema.Int64Attribute{Optional: true},
		"translated_port":                             schema.Int64Attribute{Optional: true},
		"protocol":                                    schema.StringAttribute{Optional: true},
		"fall_through":                                schema.BoolAttribute{Optional: true},
		"ipv6":                                        schema.BoolAttribute{Optional: true},
		"net_to_net":                                  schema.BoolAttribute{Optional: true},
		"no_proxy_arp":                                schema.BoolAttribute{Optional: true},
		"perform_route_lookup":                        schema.BoolAttribute{Optional: true},
		"translate_dns":                               schema.BoolAttribute{Optional: true},
		"translated_network_is_destination_interface": schema.BoolAttribute{Optional: true},
	}}
	resp.Schema = schema.Schema{
		MarkdownDescription: helpers.NewAttributeDescription(
			"Bulk-manages a tenant-scoped set of FTD auto NAT (object NAT) rules within a shared NAT policy. " +
				"Auto-NAT rules are unordered (FMC sorts them by specificity); the map key is the tenant's stable identifier. " +
				"`match_on` declares the tenant scope and auto-fills the matching fields onto items that omit them. " +
				"Cooperative ownership — other tenants' rules in the same section are ignored.").String,
		Attributes: map[string]schema.Attribute{
			"id":     schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"domain": schema.StringAttribute{Optional: true, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
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
			"rules": schema.MapNestedAttribute{Optional: true, NestedObject: autoItem},
		},
	}
}

// ─── Private helpers ─────────────────────────────────────────────────────────

func (r *MzeAutoNatRules) syntheticID() string {
	return r.FtdNatPolicyID.ValueString() + ":" + r.MatchOn.extract().Hash()
}
func (r *MzeAutoNatRules) resourcePath() string {
	return "/api/fmc_config/v1/domain/{DOMAIN_UUID}/policy/ftdnatpolicies/" + r.FtdNatPolicyID.ValueString() + "/autonatrules"
}

func (it MzeAutoNatRulesItem) fieldsForMatchOn() map[string]*string {
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

func (it *MzeAutoNatRulesItem) applyAutoFill(matcher helpers.MatchOn) {
	fields := it.fieldsForMatchOn()
	matcher.AutoFill(fields)
	if v, ok := fields["source_interface_id"]; ok && v != nil && (it.SourceInterfaceID.IsNull() || it.SourceInterfaceID.IsUnknown()) {
		it.SourceInterfaceID = types.StringValue(*v)
	}
	if v, ok := fields["destination_interface_id"]; ok && v != nil && (it.DestinationInterfaceID.IsNull() || it.DestinationInterfaceID.IsUnknown()) {
		it.DestinationInterfaceID = types.StringValue(*v)
	}
}

func (it MzeAutoNatRulesItem) toBody() string {
	body := `{"type":"FTDAutoNatRule"}`
	body, _ = sjson.Set(body, "natType", it.NatType.ValueString())
	body, _ = sjson.Set(body, "originalNetwork.id", it.OriginalNetworkID.ValueString())
	if !it.TranslatedNetworkID.IsNull() && it.TranslatedNetworkID.ValueString() != "" {
		body, _ = sjson.Set(body, "translatedNetwork.id", it.TranslatedNetworkID.ValueString())
	}
	if !it.SourceInterfaceID.IsNull() && it.SourceInterfaceID.ValueString() != "" {
		body, _ = sjson.Set(body, "sourceInterface.id", it.SourceInterfaceID.ValueString())
	}
	if !it.DestinationInterfaceID.IsNull() && it.DestinationInterfaceID.ValueString() != "" {
		body, _ = sjson.Set(body, "destinationInterface.id", it.DestinationInterfaceID.ValueString())
	}
	if !it.OriginalPort.IsNull() {
		body, _ = sjson.Set(body, "originalPort", it.OriginalPort.ValueInt64())
	}
	if !it.TranslatedPort.IsNull() {
		body, _ = sjson.Set(body, "translatedPort", it.TranslatedPort.ValueInt64())
	}
	if !it.Protocol.IsNull() && it.Protocol.ValueString() != "" {
		body, _ = sjson.Set(body, "serviceProtocol", it.Protocol.ValueString())
	}
	for _, b := range []struct {
		path string
		v    types.Bool
	}{
		{"fallThrough", it.FallThrough}, {"interfaceIpv6", it.IPv6}, {"netToNet", it.NetToNet},
		{"noProxyArp", it.NoProxyArp}, {"routeLookup", it.PerformRouteLookup}, {"dns", it.TranslateDNS},
		{"interfaceInTranslatedNetwork", it.TranslatedNetworkIsDestinationInterface},
	} {
		if !b.v.IsNull() && !b.v.IsUnknown() {
			body, _ = sjson.Set(body, b.path, b.v.ValueBool())
		}
	}
	return body
}

func (it *MzeAutoNatRulesItem) fromBody(res gjson.Result) {
	it.ID = types.StringValue(res.Get("id").String())
	if v := res.Get("natType"); v.Exists() {
		it.NatType = types.StringValue(v.String())
	}
	if v := res.Get("originalNetwork.id"); v.Exists() {
		it.OriginalNetworkID = types.StringValue(v.String())
	}
	if v := res.Get("translatedNetwork.id"); v.Exists() {
		it.TranslatedNetworkID = types.StringValue(v.String())
	}
	if v := res.Get("sourceInterface.id"); v.Exists() {
		it.SourceInterfaceID = types.StringValue(v.String())
	}
	if v := res.Get("destinationInterface.id"); v.Exists() {
		it.DestinationInterfaceID = types.StringValue(v.String())
	}
	if v := res.Get("originalPort"); v.Exists() {
		it.OriginalPort = types.Int64Value(v.Int())
	}
	if v := res.Get("translatedPort"); v.Exists() {
		it.TranslatedPort = types.Int64Value(v.Int())
	}
	if v := res.Get("serviceProtocol"); v.Exists() {
		it.Protocol = types.StringValue(v.String())
	}
	for _, b := range []struct {
		path string
		dst  *types.Bool
	}{
		{"fallThrough", &it.FallThrough}, {"interfaceIpv6", &it.IPv6}, {"netToNet", &it.NetToNet},
		{"noProxyArp", &it.NoProxyArp}, {"routeLookup", &it.PerformRouteLookup}, {"dns", &it.TranslateDNS},
		{"interfaceInTranslatedNetwork", &it.TranslatedNetworkIsDestinationInterface},
	} {
		if v := res.Get(b.path); v.Exists() {
			*b.dst = types.BoolValue(v.Bool())
		}
	}
}

// ─── CRUD ────────────────────────────────────────────────────────────────────

func (r *MzeAutoNatRulesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state MzeAutoNatRules
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for k, it := range state.Rules {
		if it.ID.IsNull() || it.ID.ValueString() == "" {
			continue
		}
		res, err := r.client.Get(state.resourcePath()+"/"+it.ID.ValueString(),
			fmc.DomainName(state.Domain.ValueString()))
		if err != nil {
			if strings.Contains(err.Error(), "StatusCode 404") {
				tflog.Debug(ctx, "auto-NAT rule "+it.ID.ValueString()+" gone on FMC, dropping from state")
				delete(state.Rules, k)
				continue
			}
			resp.Diagnostics.AddError("FMC GET failed", err.Error())
			return
		}
		it.fromBody(res)
		state.Rules[k] = it
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *MzeAutoNatRulesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan MzeAutoNatRules
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	matcher := plan.MatchOn.extract()
	if matcher.IsEmpty() {
		resp.Diagnostics.AddError("invalid match_on", "match_on must declare at least one field")
		return
	}
	for k, it := range plan.Rules {
		if err := matcher.Validate(k, it.fieldsForMatchOn()); err != nil {
			resp.Diagnostics.AddError("match_on validation failed", err.Error())
			return
		}
		it.applyAutoFill(matcher)
		body := it.toBody()
		postRes, postErr := r.client.Post(plan.resourcePath(), body, fmc.DomainName(plan.Domain.ValueString()))
		if postErr != nil {
			rec, recErr := helpers.FindDuplicateAutoNatRule(ctx, r.client, plan.resourcePath(),
				it.OriginalNetworkID.ValueString(), postErr, postRes,
				fmc.DomainName(plan.Domain.ValueString()))
			if recErr != nil {
				resp.Diagnostics.AddError("auto-NAT POST failed for key "+k, recErr.Error())
				return
			}
			it.fromBody(rec)
		} else {
			it.fromBody(postRes)
		}
		plan.Rules[k] = it
	}
	plan.ID = types.StringValue(plan.syntheticID())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *MzeAutoNatRulesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state MzeAutoNatRules
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	matcher := plan.MatchOn.extract()

	// removed
	for k, oldIt := range state.Rules {
		if _, present := plan.Rules[k]; !present {
			_, err := r.client.Delete(state.resourcePath()+"/"+oldIt.ID.ValueString(),
				fmc.DomainName(state.Domain.ValueString()))
			if err != nil && !strings.Contains(err.Error(), "StatusCode 404") {
				resp.Diagnostics.AddError("DELETE failed for key "+k, err.Error())
				return
			}
		}
	}

	// added or modified
	for k, newIt := range plan.Rules {
		oldIt, present := state.Rules[k]
		newIt.applyAutoFill(matcher)
		newBody := newIt.toBody()
		switch {
		case !present:
			// added — POST with duplicate recovery
			postRes, postErr := r.client.Post(plan.resourcePath(), newBody,
				fmc.DomainName(plan.Domain.ValueString()))
			if postErr != nil {
				rec, recErr := helpers.FindDuplicateAutoNatRule(ctx, r.client, plan.resourcePath(),
					newIt.OriginalNetworkID.ValueString(), postErr, postRes,
					fmc.DomainName(plan.Domain.ValueString()))
				if recErr != nil {
					resp.Diagnostics.AddError("POST failed for key "+k, recErr.Error())
					return
				}
				newIt.fromBody(rec)
			} else {
				newIt.fromBody(postRes)
			}
		case oldIt.toBody() != newBody:
			// modified — PUT by stored id
			id := oldIt.ID.ValueString()
			b, _ := sjson.Set(newBody, "id", id)
			res, err := r.client.Put(plan.resourcePath()+"/"+id, b,
				fmc.DomainName(plan.Domain.ValueString()))
			if err != nil {
				resp.Diagnostics.AddError("PUT failed for key "+k, err.Error())
				return
			}
			newIt.fromBody(res)
		default:
			// unchanged — keep existing id
			newIt.ID = oldIt.ID
		}
		plan.Rules[k] = newIt
	}
	plan.ID = types.StringValue(plan.syntheticID())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *MzeAutoNatRulesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state MzeAutoNatRules
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for k, it := range state.Rules {
		if it.ID.IsNull() || it.ID.ValueString() == "" {
			continue
		}
		_, err := r.client.Delete(state.resourcePath()+"/"+it.ID.ValueString(),
			fmc.DomainName(state.Domain.ValueString()))
		if err != nil && !strings.Contains(err.Error(), "StatusCode 404") {
			resp.Diagnostics.AddError("DELETE failed for key "+k, err.Error())
			return
		}
	}
}

func (r *MzeAutoNatRulesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, ":", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError("invalid import id",
			"expected <ftd_nat_policy_id>:<match_on_hash>, got "+req.ID)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("ftd_nat_policy_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
