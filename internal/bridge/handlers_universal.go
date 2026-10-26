// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zmcp/odata-mcp/internal/mcp"
	"github.com/zmcp/odata-mcp/internal/models"
)

// generateUniversalTool creates a single universal OData tool
// This is used when --universal flag is set to reduce tool count for large services
func (b *ODataMCPBridge) generateUniversalTool() {
	toolName := b.formatToolName("OData", "")

	description := b.generateUniversalDescription()

	tool := &mcp.Tool{
		Name:        toolName,
		Description: description,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"description": "Operation: info|list|get|create|update|delete|count|search|call",
					"enum":        []string{"info", "list", "get", "create", "update", "delete", "count", "search", "call"},
				},
				"target": map[string]any{
					"type":        "string",
					"description": "Entity set name (e.g., 'Products') or function name; omit for action=info",
				},
				"params": map[string]any{
					"type":        "object",
					"description": "Action-specific parameters (filter, select, expand, orderby, top, skip, key, data, method)",
				},
			},
			"required": []string{"action"},
		},
	}

	handler := func(ctx context.Context, args map[string]any) (any, error) {
		return b.handleUniversalTool(ctx, args)
	}

	b.registerTool(tool, handler, &models.ToolInfo{
		Name:        toolName,
		Description: description,
		Operation:   "universal",
	})
}

// generateUniversalDescription generates a description listing all entities and functions
func (b *ODataMCPBridge) generateUniversalDescription() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("OData service: %s\n\n", b.config.ServiceURL))

	// List entities with capabilities (sorted)
	sb.WriteString("Entities:\n")
	entityNames := make([]string, 0, len(b.metadata.EntitySets))
	for name := range b.metadata.EntitySets {
		if b.shouldIncludeEntity(name) {
			entityNames = append(entityNames, name)
		}
	}
	sort.Strings(entityNames)

	for _, name := range entityNames {
		es := b.metadata.EntitySets[name]
		ops := []string{"list", "get", "count"}
		if es.Searchable {
			ops = append(ops, "search")
		}
		if es.Creatable && !b.config.IsReadOnly() {
			ops = append(ops, "create")
		}
		if es.Updatable && !b.config.IsReadOnly() {
			ops = append(ops, "update")
		}
		if es.Deletable && !b.config.IsReadOnly() {
			ops = append(ops, "delete")
		}
		sb.WriteString(fmt.Sprintf("  %s [%s]\n", name, strings.Join(ops, ",")))
	}

	// List functions (sorted)
	functionNames := make([]string, 0, len(b.metadata.FunctionImports))
	for name := range b.metadata.FunctionImports {
		if b.shouldIncludeFunction(name) {
			functionNames = append(functionNames, name)
		}
	}

	if len(functionNames) > 0 {
		sort.Strings(functionNames)
		sb.WriteString("\nFunctions:\n")
		for _, name := range functionNames {
			fn := b.metadata.FunctionImports[name]
			// Skip modifying functions in read-only mode
			if b.config.ReadOnly || (!b.config.AllowModifyingFunctions() && b.isFunctionModifying(fn)) {
				continue
			}
			if !b.isCallableBinding(fn) {
				continue
			}
			sb.WriteString(fmt.Sprintf("  %s(%s)\n", name, b.formatFunctionParams(fn)))
		}
	}

	// Usage examples
	sb.WriteString(`
Actions:
  info   - Without target: service details and hints. With target: that
           entity's properties, types, key and operations. Call it before
           guessing a field name.
  list   - Query entities with filter/select/expand/orderby/top/skip
  get    - Retrieve single entity by key
  create - Create new entity
  update - Update existing entity (method: PUT|PATCH|MERGE)
  delete - Delete entity by key
  count  - Count entities matching filter
  search - Full-text search (if supported)
  call   - Execute function/action

Examples:
  action="list" target="Products" params={"filter":"Price gt 100","top":10}
  action="get" target="Products" params={"key":{"ID":123}}
  action="create" target="Orders" params={"data":{"CustomerID":"C001"}}
  action="call" target="ReleaseOrder" params={"OrderID":"O001"}
  action="call" target="Orders_Cancel" params={"key":{"ID":123},"reason":"duplicate"} - bound to one entity
  action="info" target="Products" - properties, key and operations of Products
  action="info" (no target) - service details and these hints again
`)

	// Universal mode registers no separate service info tool, so hints reach
	// the caller here or not at all.
	if hintText := b.hintManager.Text(b.config.ServiceURL); hintText != "" {
		sb.WriteString("\nService hints:\n")
		sb.WriteString(hintText)
	}

	return sb.String()
}

// formatFunctionParams formats function parameters for the description
func (b *ODataMCPBridge) formatFunctionParams(fn *models.FunctionImport) string {
	var params []string
	if target, err := b.boundTarget(fn); err == nil && target != nil && target.keyArg != "" {
		params = append(params, fmt.Sprintf("%s: %s key", target.keyArg, target.entitySet))
	}
	for _, p := range fn.Parameters {
		if p.Mode == "In" || p.Mode == "InOut" {
			params = append(params, fmt.Sprintf("%s: %s", p.Name, p.Type))
		}
	}
	return strings.Join(params, ", ")
}

// handleUniversalTool routes universal tool calls to appropriate handlers
func (b *ODataMCPBridge) handleUniversalTool(ctx context.Context, args map[string]any) (any, error) {
	action, ok := args["action"].(string)
	if !ok {
		return nil, fmt.Errorf("missing required parameter: action")
	}

	target, _ := args["target"].(string)

	if action == "info" {
		if target == "" {
			return b.handleUniversalServiceInfo()
		}
		return b.handleUniversalTargetInfo(target)
	}

	if target == "" {
		return nil, fmt.Errorf("missing required parameter: target")
	}

	// Get params (optional)
	params, _ := args["params"].(map[string]any)
	if params == nil {
		params = make(map[string]any)
	}

	// A target the operator filtered out is treated exactly like one that does
	// not exist, so --entities and --functions hold at call time, not just in
	// the description.
	entitySet, isEntity := b.metadata.EntitySets[target]
	if isEntity && !b.shouldIncludeEntity(target) {
		isEntity = false
	}
	function, isFunction := b.metadata.FunctionImports[target]
	if isFunction && !b.shouldIncludeFunction(target) {
		isFunction = false
	}

	if !isEntity && !isFunction {
		return nil, fmt.Errorf("unknown target: %s (not an entity set or function)", target)
	}

	if op, known := universalActionOps[action]; known && !b.config.IsOperationEnabled(op) {
		return nil, fmt.Errorf("%s operation is disabled on this server", action)
	}

	if isEntity {
		if err := b.refuseNavigation(entitySet, params); err != nil {
			return nil, err
		}
	}

	// Route to appropriate handler
	switch action {
	case "list":
		if !isEntity {
			return nil, fmt.Errorf("%s is not an entity set", target)
		}
		return b.handleEntityFilter(ctx, target, params)

	case "get":
		if !isEntity {
			return nil, fmt.Errorf("%s is not an entity set", target)
		}
		entityType := b.lookupEntityType(entitySet.EntityType)
		if entityType == nil {
			return nil, fmt.Errorf("entity type not found for %s", target)
		}
		// Extract key from params
		if keyObj, ok := params["key"].(map[string]any); ok {
			// Merge key into params for the handler
			for k, v := range keyObj {
				params[k] = v
			}
		}
		return b.handleEntityGet(ctx, target, entityType, params)

	case "create":
		if !isEntity {
			return nil, fmt.Errorf("%s is not an entity set", target)
		}
		if !entitySet.Creatable {
			return nil, fmt.Errorf("%s does not support create", target)
		}
		if b.config.IsReadOnly() {
			return nil, fmt.Errorf("create operation not allowed in read-only mode")
		}
		// Extract data from params
		if dataObj, ok := params["data"].(map[string]any); ok {
			return b.handleEntityCreate(ctx, target, dataObj)
		}
		return b.handleEntityCreate(ctx, target, params)

	case "update":
		if !isEntity {
			return nil, fmt.Errorf("%s is not an entity set", target)
		}
		if !entitySet.Updatable {
			return nil, fmt.Errorf("%s does not support update", target)
		}
		if b.config.IsReadOnly() {
			return nil, fmt.Errorf("update operation not allowed in read-only mode")
		}
		entityType := b.lookupEntityType(entitySet.EntityType)
		if entityType == nil {
			return nil, fmt.Errorf("entity type not found for %s", target)
		}
		// Extract key and data from params
		mergedParams := make(map[string]any)
		if keyObj, ok := params["key"].(map[string]any); ok {
			for k, v := range keyObj {
				mergedParams[k] = v
			}
		}
		if dataObj, ok := params["data"].(map[string]any); ok {
			for k, v := range dataObj {
				mergedParams[k] = v
			}
		}
		if method, ok := params["method"].(string); ok {
			mergedParams["_method"] = method
		}
		return b.handleEntityUpdate(ctx, target, entityType, mergedParams)

	case "delete":
		if !isEntity {
			return nil, fmt.Errorf("%s is not an entity set", target)
		}
		if !entitySet.Deletable {
			return nil, fmt.Errorf("%s does not support delete", target)
		}
		if b.config.IsReadOnly() {
			return nil, fmt.Errorf("delete operation not allowed in read-only mode")
		}
		entityType := b.lookupEntityType(entitySet.EntityType)
		if entityType == nil {
			return nil, fmt.Errorf("entity type not found for %s", target)
		}
		// Extract key from params
		if keyObj, ok := params["key"].(map[string]any); ok {
			for k, v := range keyObj {
				params[k] = v
			}
		}
		return b.handleEntityDelete(ctx, target, entityType, params)

	case "count":
		if !isEntity {
			return nil, fmt.Errorf("%s is not an entity set", target)
		}
		return b.handleEntityCount(ctx, target, params)

	case "search":
		if !isEntity {
			return nil, fmt.Errorf("%s is not an entity set", target)
		}
		if !entitySet.Searchable {
			return nil, fmt.Errorf("%s does not support search", target)
		}
		return b.handleEntitySearch(ctx, target, params)

	case "call":
		if !isFunction {
			return nil, fmt.Errorf("%s is not a function", target)
		}
		// Check read-only mode for modifying functions
		if b.config.ReadOnly || (!b.config.AllowModifyingFunctions() && b.isFunctionModifying(function)) {
			return nil, fmt.Errorf("function %s not allowed in read-only mode", target)
		}
		return b.handleFunctionCall(ctx, target, function, params)

	default:
		return nil, fmt.Errorf("unknown action: %s (valid: list, get, create, update, delete, count, search, call)", action)
	}
}

// refuseNavigation keeps --entities meaningful: a query option that walks a
// navigation property reaches an entity set the filter was meant to hide.
func (b *ODataMCPBridge) refuseNavigation(entitySet *models.EntitySet, params map[string]any) error {
	if len(b.config.AllowedEntities) == 0 {
		return nil
	}

	entityType := b.lookupEntityType(entitySet.EntityType)
	if entityType == nil {
		return nil
	}

	for key, value := range params {
		option := b.mapParameterToOData(key)
		switch option {
		case "$filter", "$select", "$expand", "$orderby":
		default:
			continue
		}

		text, _ := value.(string)
		for _, nav := range entityType.NavigationProps {
			if containsIdentifier(text, nav.Name) {
				return fmt.Errorf("%s uses navigation property %s, which --entities does not allow", option, nav.Name)
			}
		}
	}

	return nil
}

// containsIdentifier reports whether name appears in text as a whole token.
func containsIdentifier(text, name string) bool {
	for start := 0; ; {
		idx := strings.Index(text[start:], name)
		if idx < 0 {
			return false
		}
		idx += start
		end := idx + len(name)
		if (idx == 0 || !isIdentifierByte(text[idx-1])) && (end == len(text) || !isIdentifierByte(text[end])) {
			return true
		}
		start = idx + 1
	}
}

func isIdentifierByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// universalActionOps maps each action to the operation letter --enable and
// --disable use, matching how per-entity tools are generated.
var universalActionOps = map[string]rune{
	"list":   'F',
	"count":  'F',
	"search": 'S',
	"get":    'G',
	"create": 'C',
	"update": 'U',
	"delete": 'D',
	"call":   'A',
}

// handleUniversalTargetInfo describes one entity set or function from the
// parsed metadata, so a caller can learn field names without fetching a row.
func (b *ODataMCPBridge) handleUniversalTargetInfo(target string) (any, error) {
	var info map[string]any
	if entitySet, ok := b.metadata.EntitySets[target]; ok && b.shouldIncludeEntity(target) {
		info = b.describeEntitySet(target, entitySet)
	} else if function, ok := b.metadata.FunctionImports[target]; ok && b.shouldIncludeFunction(target) {
		info = b.describeFunction(target, function)
	} else {
		return nil, fmt.Errorf("unknown target: %s (not an entity set or function)", target)
	}

	result, err := json.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("failed to format response: %w", err)
	}

	return string(result), nil
}

func (b *ODataMCPBridge) describeEntitySet(name string, entitySet *models.EntitySet) map[string]any {
	info := map[string]any{
		"entity_set":  name,
		"entity_type": entitySet.EntityType,
		"operations":  b.entitySetOperations(entitySet),
	}

	if entityType := b.lookupEntityType(entitySet.EntityType); entityType != nil {
		info["key"] = entityType.KeyProperties

		properties := make([]map[string]any, 0, len(entityType.Properties))
		for _, prop := range entityType.Properties {
			properties = append(properties, map[string]any{
				"name":     prop.Name,
				"type":     prop.Type,
				"nullable": prop.Nullable,
			})
		}
		info["properties"] = properties

		if len(b.config.AllowedEntities) == 0 && len(entityType.NavigationProps) > 0 {
			navigation := make([]string, 0, len(entityType.NavigationProps))
			for _, nav := range entityType.NavigationProps {
				navigation = append(navigation, nav.Name)
			}
			info["navigation_properties"] = navigation
		}
	}

	if hints := b.hintManager.GetHints(b.config.ServiceURL); hints != nil {
		if entityHints, ok := hints["entity_hints"].(map[string]interface{}); ok {
			if hint, ok := entityHints[name]; ok {
				info["hints"] = hint
			}
		}
	}

	return info
}

func (b *ODataMCPBridge) describeFunction(name string, function *models.FunctionImport) map[string]any {
	parameters := make([]map[string]any, 0, len(function.Parameters))
	for _, param := range function.Parameters {
		if param.Mode == "In" || param.Mode == "InOut" {
			parameters = append(parameters, map[string]any{
				"name":     param.Name,
				"type":     param.Type,
				"nullable": param.Nullable,
			})
		}
	}

	info := map[string]any{
		"function":    name,
		"http_method": function.HTTPMethod,
		"return_type": function.ReturnType,
		"parameters":  parameters,
		"callable":    !b.config.ReadOnly && (b.config.AllowModifyingFunctions() || !b.isFunctionModifying(function)),
	}
	target, err := b.boundTarget(function)
	if err != nil {
		info["callable"] = false
		info["binding_error"] = err.Error()
		return info
	}
	if target != nil {
		info["bound_to"] = target.entitySet
		if target.keyArg != "" {
			info["key_param"] = target.keyArg
		}
	}

	return info
}

func (b *ODataMCPBridge) boundTarget(function *models.FunctionImport) (*binding, error) {
	if !function.IsBound {
		return nil, nil
	}

	return b.resolveBinding(function)
}

func (b *ODataMCPBridge) isCallableBinding(function *models.FunctionImport) bool {
	_, err := b.boundTarget(function)
	return err == nil
}

// entitySetOperations lists what a caller may do to an entity set here, which
// depends on the metadata and on the read-only and operation flags together.
func (b *ODataMCPBridge) entitySetOperations(entitySet *models.EntitySet) []string {
	ops := make([]string, 0, 7)
	add := func(action string, allowed bool) {
		if allowed && b.config.IsOperationEnabled(universalActionOps[action]) {
			ops = append(ops, action)
		}
	}

	add("list", true)
	add("get", true)
	add("count", true)
	add("search", entitySet.Searchable)
	add("create", entitySet.Creatable && !b.config.IsReadOnly())
	add("update", entitySet.Updatable && !b.config.IsReadOnly())
	add("delete", entitySet.Deletable && !b.config.IsReadOnly())

	return ops
}

// handleUniversalServiceInfo handles the service_info action for universal mode
func (b *ODataMCPBridge) handleUniversalServiceInfo() (any, error) {
	info := map[string]any{
		"service_url":      b.config.ServiceURL,
		"entity_sets":      len(b.metadata.EntitySets),
		"entity_types":     len(b.metadata.EntityTypes),
		"function_imports": len(b.metadata.FunctionImports),
		"schema_namespace": b.metadata.SchemaNamespace,
		"container_name":   b.metadata.ContainerName,
		"version":          b.metadata.Version,
		"mode":             "universal",
	}

	// Add entity list with capabilities
	entities := make(map[string][]string)
	for name, es := range b.metadata.EntitySets {
		if !b.shouldIncludeEntity(name) {
			continue
		}
		entities[name] = b.entitySetOperations(es)
	}
	info["entities"] = entities

	// Add function list
	functions := make([]string, 0)
	for name, fn := range b.metadata.FunctionImports {
		if !b.shouldIncludeFunction(name) {
			continue
		}
		if b.config.ReadOnly || (!b.config.AllowModifyingFunctions() && b.isFunctionModifying(fn)) {
			continue
		}
		functions = append(functions, name)
	}
	sort.Strings(functions)
	info["functions"] = functions

	// Add hints
	hints := b.hintManager.GetHints(b.config.ServiceURL)
	if hints != nil {
		info["implementation_hints"] = hints
	}

	response, err := json.Marshal(info)
	if err != nil {
		return "Error formatting service info", err
	}

	return string(response), nil
}
