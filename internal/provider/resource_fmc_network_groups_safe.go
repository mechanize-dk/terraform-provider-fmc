// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// All rights reserved.
//
// Licensed under the Mozilla Public License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://mozilla.org/MPL/2.0/
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/CiscoDevNet/terraform-provider-fmc/internal/provider/helpers"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/netascode/go-fmc"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// gcNamePrefix is prepended to the FMC object ID when a network group is soft-deleted
// (i.e. it is still referenced by access rules or other groups and cannot be deleted yet).
// The Read function runs garbage collection and removes these groups once they are no longer referenced.
const gcNamePrefix = "__gc_"

// gcMarkerIP is set as the sole literal on a soft-deleted group.
// 127.6.6.6 is a loopback address that makes any access rule pointing at the group harmless.
// FMC requires at least one member in a network group, so we use this as a placeholder.
const gcMarkerIP = "127.6.6.6"

var (
	_ resource.Resource                = &NetworkGroupsSafeResource{}
	_ resource.ResourceWithImportState = &NetworkGroupsSafeResource{}
)

func NewNetworkGroupsSafeResource() resource.Resource {
	return &NetworkGroupsSafeResource{}
}

type NetworkGroupsSafeResource struct {
	client *fmc.Client
}

func (r *NetworkGroupsSafeResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_network_groups_safe"
}

func (r *NetworkGroupsSafeResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: helpers.NewAttributeDescription("This resource manages Network Groups through bulk operations, with safe (soft) deletion. " +
			"When a network group cannot be deleted because it is still referenced by an access rule or another group, " +
			"it is renamed to `" + gcNamePrefix + "<id>` and its content is replaced with a single loopback literal (`" + gcMarkerIP + "`) " +
			"so that any rule pointing at it becomes harmless. " +
			"The group is fully removed from FMC the next time this resource is read (during `terraform plan` or `terraform apply`) " +
			"once it is no longer referenced. " +
			"**Note:** Do not use names starting with `" + gcNamePrefix + "` for your own network groups — they will be treated as GC candidates.").
			AddMinimumVersionHeaderDescription().
			AddMinimumVersionBulkDeleteDescription("7.4").
			AddMinimumVersionBulkDisclaimerDescription().
			AddMinimumVersionBulkUpdateDescription().String,

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Id of the object",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"domain": schema.StringAttribute{
				MarkdownDescription: "Name of the FMC domain",
				Optional:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"items": schema.MapNestedAttribute{
				MarkdownDescription: helpers.NewAttributeDescription("Map of Network Groups. The key of the map is the name of the individual Network Group.").String,
				Optional:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							MarkdownDescription: helpers.NewAttributeDescription("Id of the Network Group.").String,
							Computed:            true,
							PlanModifiers: []planmodifier.String{
								stringplanmodifier.UseNonNullStateForUnknown(),
							},
						},
						"description": schema.StringAttribute{
							MarkdownDescription: helpers.NewAttributeDescription("Description of the object.").String,
							Optional:            true,
						},
						"type": schema.StringAttribute{
							MarkdownDescription: helpers.NewAttributeDescription("Type of the object; this value is always 'NetworkGroup'.").String,
							Computed:            true,
							PlanModifiers: []planmodifier.String{
								stringplanmodifier.UseNonNullStateForUnknown(),
							},
						},
						"overridable": schema.BoolAttribute{
							MarkdownDescription: helpers.NewAttributeDescription("Whether the object values can be overridden.").String,
							Optional:            true,
						},
						"network_groups": schema.SetAttribute{
							MarkdownDescription: helpers.NewAttributeDescription("Set of names (not Ids) of child Network Groups. The names must be defined in the same instance of `fmc_network_groups_safe` resource.").String,
							ElementType:         types.StringType,
							Optional:            true,
						},
						"objects": schema.SetNestedAttribute{
							MarkdownDescription: helpers.NewAttributeDescription("Set of network objects (Hosts, Networks, Ranges, FQDNs or Network Group).").String,
							Optional:            true,
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"id": schema.StringAttribute{
										MarkdownDescription: helpers.NewAttributeDescription("Id of the network object.").String,
										Optional:            true,
									},
									"name": schema.StringAttribute{
										MarkdownDescription: helpers.NewAttributeDescription("Name of the network object.").String,
										Optional:            true,
									},
								},
							},
						},
						"literals": schema.SetNestedAttribute{
							MarkdownDescription: helpers.NewAttributeDescription("Set of literal values.").String,
							Optional:            true,
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"value": schema.StringAttribute{
										MarkdownDescription: helpers.NewAttributeDescription("IP address or network in CIDR format. Please do not use /32 mask for host.").String,
										Optional:            true,
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (r *NetworkGroupsSafeResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.client = req.ProviderData.(*FmcProviderData).Client
}

func (r *NetworkGroupsSafeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan NetworkGroups

	diags := req.Plan.Get(ctx, &plan)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	reqMods := [](func(*fmc.Req)){}
	if !plan.Domain.IsNull() && plan.Domain.ValueString() != "" {
		reqMods = append(reqMods, fmc.DomainName(plan.Domain.ValueString()))
	}

	tflog.Debug(ctx, fmt.Sprintf("%s: Beginning Create", plan.Id.ValueString()))

	body := plan.toBody(ctx, NetworkGroups{})
	plan.Id = types.StringValue(uuid.New().String())

	state := plan
	if len(plan.Items) > 0 {
		state.Items = map[string]NetworkGroupsItems{}
	}

	state, diags = r.updateSubresources(ctx, req.Plan, plan, body, tfsdk.State{}, state, reqMods...)
	resp.Diagnostics.Append(diags...)

	tflog.Debug(ctx, fmt.Sprintf("%s: Create finished", state.Id.ValueString()))

	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)

	helpers.SetFlagImporting(ctx, false, resp.Private, &resp.Diagnostics)
}

func (r *NetworkGroupsSafeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state NetworkGroups

	diags := req.State.Get(ctx, &state)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	reqMods := [](func(*fmc.Req)){}
	if !state.Domain.IsNull() && state.Domain.ValueString() != "" {
		reqMods = append(reqMods, fmc.DomainName(state.Domain.ValueString()))
	}

	tflog.Debug(ctx, fmt.Sprintf("%s: Beginning Read", state.Id.String()))

	res, err := r.client.Get(state.getPath()+"?expanded=true", reqMods...)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Failed to retrieve objects (GET), got error: %s", err))
		return
	}

	// Garbage-collect any soft-deleted groups that are no longer referenced.
	// This is a best-effort side effect: errors are logged and ignored.
	r.runGarbageCollection(ctx, state, res, reqMods...)

	res = synthesizeNetworkGroups(ctx, res, &state)

	imp, diags := helpers.IsFlagImporting(ctx, req)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	if imp {
		state.fromBody(ctx, res)
	} else {
		state.fromBodyPartial(ctx, res)
	}

	tflog.Debug(ctx, fmt.Sprintf("%s: Read finished successfully", state.Id.ValueString()))

	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)

	helpers.SetFlagImporting(ctx, false, resp.Private, &resp.Diagnostics)
}

func (r *NetworkGroupsSafeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state NetworkGroups

	diags := req.Plan.Get(ctx, &plan)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	diags = req.State.Get(ctx, &state)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	reqMods := [](func(*fmc.Req)){}
	if !plan.Domain.IsNull() && plan.Domain.ValueString() != "" {
		reqMods = append(reqMods, fmc.DomainName(plan.Domain.ValueString()))
	}

	tflog.Debug(ctx, fmt.Sprintf("%s: Beginning Update", plan.Id.ValueString()))

	body := plan.toBody(ctx, state)

	state, diags = r.updateSubresources(ctx, req.Plan, plan, body, req.State, state, reqMods...)
	resp.Diagnostics.Append(diags...)

	tflog.Debug(ctx, fmt.Sprintf("%s: Update finished successfully", plan.Id.ValueString()))

	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
}

func (r *NetworkGroupsSafeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state NetworkGroups

	diags := req.State.Get(ctx, &state)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	reqMods := [](func(*fmc.Req)){}
	if !state.Domain.IsNull() && state.Domain.ValueString() != "" {
		reqMods = append(reqMods, fmc.DomainName(state.Domain.ValueString()))
	}

	tflog.Debug(ctx, fmt.Sprintf("%s: Beginning Delete", state.Id.ValueString()))

	state, diags = r.updateSubresources(ctx, tfsdk.Plan{}, NetworkGroups{}, "{}", req.State, state, reqMods...)
	resp.Diagnostics.Append(diags...)

	diags = resp.State.Set(ctx, &state)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	resp.State.RemoveResource(ctx)
	tflog.Debug(ctx, fmt.Sprintf("%s: Delete finished", state.Id.ValueString()))
}

func (r *NetworkGroupsSafeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	var inputPattern = regexp.MustCompile(`^(?:(?P<domain>[^\s,]+),)?\[(?P<names>.*?)\]$`)
	match := inputPattern.FindStringSubmatch(req.ID)
	if match == nil {
		errMsg := "Failed to parse import parameters.\nPlease provide import string in the following format: <domain>,[<item1_name>,<item2_name>,...]\n<domain> is optional.\n" + fmt.Sprintf("Got: %q", req.ID)
		resp.Diagnostics.AddError("Import error", errMsg)
		return
	}

	if tmpDomain := match[inputPattern.SubexpIndex("domain")]; tmpDomain != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("domain"), tmpDomain)...)
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), uuid.New().String())...)

	names := strings.Split(match[inputPattern.SubexpIndex("names")], ",")
	itemsMap := make(map[string]NetworkGroupsItems, len(names))
	for _, v := range names {
		itemsMap[v] = NetworkGroupsItems{NetworkGroups: types.SetNull(types.StringType)}
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("items"), itemsMap)...)

	helpers.SetFlagImporting(ctx, true, resp.Private, &resp.Diagnostics)
}

// updateSubresources creates, updates, and deletes subresources of the NetworkGroupsSafe resource.
// On delete, if FMC returns 409 (group still in use), the group is soft-deleted instead of failing:
// it is renamed to __gc_<id> and its content is replaced with a single 127.6.6.6 literal.
// The Read function will garbage-collect these groups once they are no longer referenced.
func (r *NetworkGroupsSafeResource) updateSubresources(ctx context.Context, tfsdkPlan tfsdk.Plan, plan NetworkGroups, planBody string, tfsdkState tfsdk.State, state NetworkGroups, reqMods ...func(*fmc.Req)) (NetworkGroups, diag.Diagnostics) {
	seq, diags := graphTopologicalSeq(ctx, planBody)
	if diags.HasError() {
		return state, diags
	}

	bulks, seq := divideToBulks(ctx, seq, plan)
	if diags.HasError() {
		return state, diags
	}

	for _, bulk := range bulks {
		readable := slices.Clone(bulk.groups)
		for i := range readable {
			readable[i].json = ""
		}
		tflog.Debug(ctx, fmt.Sprintf("%s: bulk ordered for Create: %+v", plan.Id.ValueString(), readable))
	}

	for _, bulk := range bulks {
		state, diags = bulk.Create(ctx, plan, state, r.client, reqMods...)
		if diags.HasError() {
			return state, diags
		}
		for _, group := range bulk.groups {
			tmp := plan.Items[group.name]
			tmp.Id = state.Items[group.name].Id
			plan.Items[group.name] = tmp
		}
	}

	tflog.Debug(ctx, fmt.Sprintf("%s: considering remaining subresources for Update: %+v", plan.Id.ValueString(), seq))
	for _, group := range seq {
		ok, diags := helpers.IsConfigUpdatingAt(ctx, tfsdkPlan, tfsdkState, path.Root("items").AtMapKey(group.name))
		if diags.HasError() {
			return state, diags
		}
		if !ok {
			continue
		}

		updating := plan.Items[group.name].Id.ValueString()
		tflog.Debug(ctx, fmt.Sprintf("%s: Subresource %s: Beginning Update", updating, group.name))

		body, diags := group.Body(ctx, plan)
		if diags.HasError() {
			return state, diags
		}

		res, err := r.client.Put(plan.getPath()+"/"+url.QueryEscape(updating), body, reqMods...)
		if err != nil {
			return state, diag.Diagnostics{
				diag.NewErrorDiagnostic("Client Error", fmt.Sprintf("Failed to configure object (PUT), got error: %s, %s", err, res.String())),
			}
		}

		state.Items[group.name] = plan.Items[group.name]
		tflog.Debug(ctx, fmt.Sprintf("%s: Subresource %s: Update finished successfully", updating, group.name))
	}

	// Delete subresources that are no longer in the plan.
	stateBody := state.toBody(ctx, NetworkGroups{})
	delSeq, diags := graphTopologicalSeq(ctx, stateBody)
	if diags.HasError() {
		return state, diags
	}

	if r.client.FMCVersionParsed.LessThan(minFMCVersionBulkDeleteNetworkGroups) {
		// Non-bulk path: delete one by one, reverse topological order (parents before children).
		for i := len(delSeq) - 1; i >= 0; i-- {
			gn := delSeq[i].name
			if _, found := plan.Items[gn]; found {
				continue
			}
			if state.Items[gn].Id.IsNull() {
				delete(state.Items, gn)
				continue
			}

			deleting := state.Items[gn].Id.ValueString()
			tflog.Debug(ctx, fmt.Sprintf("%s: Subresource %s: Beginning Delete", deleting, gn))

			res, err := r.client.Delete(state.getPath()+"/"+url.QueryEscape(deleting), reqMods...)
			if err != nil {
				if strings.Contains(err.Error(), "StatusCode 409") || strings.Contains(err.Error(), "StatusCode 400") {
					if softDiags := r.softDelete(ctx, state, gn, deleting, reqMods...); softDiags.HasError() {
						return state, softDiags
					}
					delete(state.Items, gn)
					continue
				}
				return state, diag.Diagnostics{
					diag.NewErrorDiagnostic("Client Error", fmt.Sprintf("Failed to delete object (DELETE), got error: %s, %s", err, res.String())),
				}
			}

			delete(state.Items, gn)
			tflog.Debug(ctx, fmt.Sprintf("%s: Subresource %s: Delete finished successfully", deleting, gn))
		}
	} else {
		// Bulk-delete path (FMC >= 7.4).
		var idsToRemove strings.Builder
		var namesToRemove []string
		var deleteGroups []networkGroupsBulkDelete

		tflog.Debug(ctx, fmt.Sprintf("%s: Bulk Delete of subresources: Beginning", plan.Id.ValueString()))

		for i := len(delSeq) - 1; i >= 0; i-- {
			gn := delSeq[i].name
			if _, found := plan.Items[gn]; !found {
				if state.Items[gn].Id.IsNull() {
					delete(state.Items, gn)
					continue
				}
				idsToRemove.WriteString(state.Items[gn].Id.ValueString())
				idsToRemove.WriteString(",")
				namesToRemove = append(namesToRemove, gn)
			}

			if (i == 0 || delSeq[i].bulk != delSeq[i-1].bulk || idsToRemove.Len() >= maxUrlParamLength) && idsToRemove.Len() > 0 {
				deleteGroups = append(deleteGroups, networkGroupsBulkDelete{
					ids:   idsToRemove.String(),
					names: slices.Clone(namesToRemove),
				})
				idsToRemove.Reset()
				namesToRemove = namesToRemove[:0]
			}
		}

		for _, group := range deleteGroups {
			urlPath := state.getPath() + "?bulk=true&filter=ids:" + url.QueryEscape(group.ids)
			_, err := r.client.Delete(urlPath, reqMods...)
			if err != nil {
				if !strings.Contains(err.Error(), "StatusCode 409") && !strings.Contains(err.Error(), "StatusCode 400") {
					return state, diag.Diagnostics{
						diag.NewErrorDiagnostic("Client Error", fmt.Sprintf("Failed to bulk delete objects (DELETE), got error: %s", err)),
					}
				}
				// Bulk failed with 409 — fall back to individual deletes with soft-delete.
				tflog.Debug(ctx, fmt.Sprintf("%s: Bulk delete conflict, falling back to individual deletes", plan.Id.ValueString()))
				for _, name := range group.names {
					id := state.Items[name].Id.ValueString()
					res, delErr := r.client.Delete(state.getPath()+"/"+url.QueryEscape(id), reqMods...)
					if delErr != nil {
						if strings.Contains(delErr.Error(), "StatusCode 409") || strings.Contains(delErr.Error(), "StatusCode 400") {
							if softDiags := r.softDelete(ctx, state, name, id, reqMods...); softDiags.HasError() {
								return state, softDiags
							}
						} else {
							return state, diag.Diagnostics{
								diag.NewErrorDiagnostic("Client Error", fmt.Sprintf("Failed to delete network group %q (DELETE), got error: %s, %s", name, delErr, res.String())),
							}
						}
					}
					delete(state.Items, name)
				}
				continue
			}

			for _, name := range group.names {
				delete(state.Items, name)
			}
		}

		tflog.Debug(ctx, fmt.Sprintf("%s: Bulk Delete of subresources: finished", plan.Id.ValueString()))
	}

	return state, nil
}

// softDelete renames the network group to __gc_<id> and replaces its content with a single
// 127.6.6.6 literal, making any access rule that references it harmless.
// The group is removed from Terraform state immediately; the GC pass in Read will delete it
// from FMC once it is no longer referenced.
func (r *NetworkGroupsSafeResource) softDelete(ctx context.Context, state NetworkGroups, name, id string, reqMods ...func(*fmc.Req)) diag.Diagnostics {
	gcName := gcNamePrefix + id
	gcBody := "{}"
	gcBody, _ = sjson.Set(gcBody, "id", id)
	gcBody, _ = sjson.Set(gcBody, "name", gcName)
	gcBody, _ = sjson.Set(gcBody, "description", "GC: was "+name)
	gcBody, _ = sjson.SetRaw(gcBody, "objects", "[]")
	gcBody, _ = sjson.SetRaw(gcBody, "literals", `[{"value":"`+gcMarkerIP+`","type":"Host"}]`)

	_, err := r.client.Put(state.getPath()+"/"+url.QueryEscape(id), gcBody, reqMods...)
	if err != nil {
		return diag.Diagnostics{
			diag.NewErrorDiagnostic("Client Error",
				fmt.Sprintf("Network group %q (id=%s) is in use and cannot be deleted; "+
					"attempted to mark it for GC as %q but the rename failed: %s", name, id, gcName, err)),
		}
	}

	tflog.Debug(ctx, fmt.Sprintf("Network group %q (id=%s) is in use; soft-deleted as %q with literal %s", name, id, gcName, gcMarkerIP))
	return nil
}

// runGarbageCollection scans the full list of network groups returned by FMC and deletes any
// that have been soft-deleted (name starts with __gc_) and are no longer referenced by anyone.
// It is called from Read so it runs on every terraform plan/apply cycle.
// Errors are logged and ignored — GC is best-effort and will retry on the next cycle.
func (r *NetworkGroupsSafeResource) runGarbageCollection(ctx context.Context, state NetworkGroups, allGroups gjson.Result, reqMods ...func(*fmc.Req)) {
	for _, v := range allGroups.Get("items").Array() {
		name := v.Get("name").String()
		if !strings.HasPrefix(name, gcNamePrefix) {
			continue
		}

		id := v.Get("id").String()
		tflog.Debug(ctx, fmt.Sprintf("GC: attempting to delete soft-deleted group %q (id=%s)", name, id))

		_, err := r.client.Delete(state.getPath()+"/"+url.QueryEscape(id), reqMods...)
		if err != nil {
			tflog.Debug(ctx, fmt.Sprintf("GC: group %q (id=%s) still in use or error — will retry next cycle: %s", name, id, err))
		} else {
			tflog.Debug(ctx, fmt.Sprintf("GC: successfully deleted soft-deleted group %q (id=%s)", name, id))
		}
	}
}
