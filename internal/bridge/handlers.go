// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zmcp/odata-mcp/internal/constants"
	"github.com/zmcp/odata-mcp/internal/models"
	"github.com/zmcp/odata-mcp/internal/utils"
)

// wrapKeyValueForType gives a key value the type its property declares, so
// the client can write the typed literal (guid'...', datetime'...') that
// OData V3 requires in a key predicate.
func wrapKeyValueForType(value any, propType string) any {
	strVal, ok := value.(string)
	if !ok {
		return value
	}

	switch propType {
	case "Edm.Guid":
		return models.GUIDValue(strVal)
	case "Edm.DateTime":
		return models.DateTimeValue(strVal)
	}

	return value
}

// getPropertyType looks up the type of a property in an entity type
func getPropertyType(entityType *models.EntityType, propName string) string {
	for _, prop := range entityType.Properties {
		if prop.Name == propName {
			return prop.Type
		}
	}
	return ""
}

func (b *ODataMCPBridge) handleServiceInfo(_ context.Context, args map[string]any) (any, error) {
	includeMetadata := false
	if val, ok := args["include_metadata"].(bool); ok {
		includeMetadata = val
	}

	info := map[string]any{
		"service_url":      b.config.ServiceURL,
		"entity_sets":      len(b.metadata.EntitySets),
		"entity_types":     len(b.metadata.EntityTypes),
		"function_imports": len(b.metadata.FunctionImports),
		"schema_namespace": b.metadata.SchemaNamespace,
		"container_name":   b.metadata.ContainerName,
		"version":          b.metadata.Version,
		"parsed_at":        b.metadata.ParsedAt.Format("2006-01-02T15:04:05Z"),
	}

	// Add service-specific hints from hint manager
	hints := b.hintManager.GetHints(b.config.ServiceURL)
	if hints != nil {
		info["implementation_hints"] = hints
	}

	if includeMetadata {
		info["entity_sets_detail"] = b.metadata.EntitySets
		info["entity_types_detail"] = b.metadata.EntityTypes
		info["function_imports_detail"] = b.metadata.FunctionImports
	}

	response, err := json.Marshal(info)
	if err != nil {
		return "Error formatting service info", err
	}

	return string(response), nil
}

func (b *ODataMCPBridge) handleEntityFilter(ctx context.Context, entitySetName string, args map[string]any) (any, error) {
	// Build query options from arguments using standard OData parameters
	options := make(map[string]string)

	// Map arguments to handle both Claude-friendly and standard parameter names
	mappedArgs := make(map[string]any)
	for key, value := range args {
		mappedKey := b.mapParameterToOData(key)
		mappedArgs[mappedKey] = value
	}

	// Handle each OData parameter
	if filter, ok := mappedArgs["$filter"].(string); ok && filter != "" {
		// Transform filter for SAP GUID formatting if needed
		filter = b.transformFilterForSAP(filter, entitySetName)
		options[constants.QueryFilter] = filter
	}
	if selectParam, ok := mappedArgs["$select"].(string); ok && selectParam != "" {
		options[constants.QuerySelect] = selectParam
	}
	if expand, ok := mappedArgs["$expand"].(string); ok && expand != "" {
		options[constants.QueryExpand] = expand
	}
	if orderby, ok := mappedArgs["$orderby"].(string); ok && orderby != "" {
		options[constants.QueryOrderBy] = orderby
	}
	if top, ok := integerOption(mappedArgs["$top"]); ok {
		options[constants.QueryTop] = top
	}
	if skip, ok := integerOption(mappedArgs["$skip"]); ok {
		options[constants.QuerySkip] = skip
	}

	// Handle $count parameter - translate to appropriate version-specific parameter
	if count, ok := mappedArgs["$count"].(bool); ok && count {
		// The client will automatically translate this to $count=true for v4
		options[constants.QueryInlineCount] = "allpages"
	}

	// Call OData client to get entity set
	response, err := b.client.GetEntitySet(ctx, entitySetName, options)
	if err != nil {
		if b.config.VerboseErrors {
			return nil, fmt.Errorf("failed to filter entities from %s with options %v: %w", entitySetName, options, err)
		}
		return nil, fmt.Errorf("failed to filter entities: %w", err)
	}

	// Enhance response based on configuration
	enhancedResponse := b.enhanceResponse(response, options)

	// Format response as JSON string
	result, err := json.Marshal(enhancedResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to format response: %w", err)
	}

	return string(result), nil
}

// integerOption reads $top or $skip whether the model sent a number or a
// numeric string. Silently dropping a string "50" meant fetching everything.
func integerOption(value any) (string, bool) {
	switch v := value.(type) {
	case float64:
		return fmt.Sprintf("%d", int(v)), true
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return fmt.Sprintf("%d", n), true
		}
	}

	return "", false
}

func (b *ODataMCPBridge) handleEntityCount(ctx context.Context, entitySetName string, args map[string]any) (any, error) {
	// Build query options - for count we typically only need filter
	options := make(map[string]string)

	// Map arguments to handle both Claude-friendly and standard parameter names
	mappedArgs := make(map[string]any)
	for key, value := range args {
		mappedKey := b.mapParameterToOData(key)
		mappedArgs[mappedKey] = value
	}

	if filter, ok := mappedArgs["$filter"].(string); ok && filter != "" {
		// Transform filter for SAP GUID formatting if needed
		filter = b.transformFilterForSAP(filter, entitySetName)
		options[constants.QueryFilter] = filter
	}

	// Add $inlinecount=allpages to get inline count (OData v2 syntax)
	options[constants.QueryInlineCount] = "allpages"
	options[constants.QueryTop] = "0" // We only want the count, not the data

	// Call OData client to get count
	response, err := b.client.GetEntitySet(ctx, entitySetName, options)
	if err != nil {
		return nil, fmt.Errorf("failed to get entity count: %w", err)
	}

	// Extract count from response
	count := int64(0)
	if response.Count != nil {
		count = *response.Count
	}

	// Return count as formatted string
	return fmt.Sprintf(`{"count": %d}`, count), nil
}

func (b *ODataMCPBridge) handleEntitySearch(ctx context.Context, entitySetName string, args map[string]any) (any, error) {
	// Get search term
	searchTerm, ok := args["search"].(string)
	if !ok {
		searchTerm, ok = args["search_term"].(string)
		if !ok {
			return nil, fmt.Errorf("missing required parameter: search_term")
		}
	}

	// Build query options
	options := make(map[string]string)
	options[constants.QuerySearch] = searchTerm

	// Map arguments to handle both Claude-friendly and standard parameter names
	mappedArgs := make(map[string]any)
	for key, value := range args {
		mappedKey := b.mapParameterToOData(key)
		mappedArgs[mappedKey] = value
	}

	// Handle optional parameters
	if top, ok := mappedArgs["$top"].(float64); ok {
		options[constants.QueryTop] = fmt.Sprintf("%d", int(top))
	}
	if skip, ok := mappedArgs["$skip"].(float64); ok {
		options[constants.QuerySkip] = fmt.Sprintf("%d", int(skip))
	}
	if selectParam, ok := mappedArgs["$select"].(string); ok && selectParam != "" {
		options[constants.QuerySelect] = selectParam
	}

	// Call OData client to search entities
	response, err := b.client.GetEntitySet(ctx, entitySetName, options)
	if err != nil {
		return nil, fmt.Errorf("failed to search entities: %w", err)
	}

	// Format response as JSON string
	result, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("failed to format response: %w", err)
	}

	return string(result), nil
}

func (b *ODataMCPBridge) handleEntityGet(ctx context.Context, entitySetName string, entityType *models.EntityType, args map[string]any) (any, error) {
	// Build key values from arguments, wrapping GUIDs appropriately
	key := make(map[string]any)
	for _, keyProp := range entityType.KeyProperties {
		if value, exists := args[keyProp]; exists {
			propType := getPropertyType(entityType, keyProp)
			key[keyProp] = wrapKeyValueForType(value, propType)
		} else {
			return nil, fmt.Errorf("missing required key property: %s", keyProp)
		}
	}

	// Map arguments to handle both Claude-friendly and standard parameter names
	mappedArgs := make(map[string]any)
	for k, value := range args {
		mappedKey := b.mapParameterToOData(k)
		mappedArgs[mappedKey] = value
	}

	// Build query options for expand/select
	options := make(map[string]string)
	if selectParam, ok := mappedArgs["$select"].(string); ok && selectParam != "" {
		options[constants.QuerySelect] = selectParam
	}
	if expand, ok := mappedArgs["$expand"].(string); ok && expand != "" {
		options[constants.QueryExpand] = expand
	}

	// Call OData client to get entity
	response, err := b.client.GetEntity(ctx, entitySetName, key, options)
	if err != nil {
		return nil, fmt.Errorf("failed to get entity: %w", err)
	}

	// Format response as JSON string
	result, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("failed to format response: %w", err)
	}

	return string(result), nil
}

func (b *ODataMCPBridge) handleEntityCreate(ctx context.Context, entitySetName string, args map[string]any) (any, error) {
	// All arguments are the entity data (excluding system parameters)
	entityData := make(map[string]any)
	for k, v := range args {
		// Skip any system parameters (starting with $)
		if !strings.HasPrefix(k, "$") {
			entityData[k] = v
		}
	}

	// Convert numeric fields to strings for SAP OData v2 compatibility
	// This prevents "Failed to read property 'Quantity' at offset" errors
	entityData = utils.ConvertNumericsInMap(entityData)

	// Convert date fields to OData legacy format if needed
	if b.config.LegacyDates {
		entityData = utils.ConvertDatesInMap(entityData, false) // false = convert ISO to legacy
	}

	// Call OData client to create entity
	response, err := b.client.CreateEntity(ctx, entitySetName, entityData)
	if err != nil {
		return nil, fmt.Errorf("failed to create entity: %w", err)
	}

	// Enhance response (includes date conversion if enabled)
	response = b.enhanceResponse(response, make(map[string]string))

	// Format response as JSON string
	result, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("failed to format response: %w", err)
	}

	return string(result), nil
}

func (b *ODataMCPBridge) handleEntityUpdate(ctx context.Context, entitySetName string, entityType *models.EntityType, args map[string]any) (any, error) {
	// Extract key values and method
	key := make(map[string]any)
	updateData := make(map[string]any)
	method := constants.PUT // default method

	for k, v := range args {
		if k == "_method" {
			if m, ok := v.(string); ok {
				method = m
			}
			continue
		}

		// Check if this is a key property
		if slices.Contains(entityType.KeyProperties, k) {
			propType := getPropertyType(entityType, k)
			key[k] = wrapKeyValueForType(v, propType)
		} else if !strings.HasPrefix(k, "$") {
			// If not a key, it's update data
			updateData[k] = v
		}
	}

	// Verify we have all required key properties
	for _, keyProp := range entityType.KeyProperties {
		if _, exists := key[keyProp]; !exists {
			return nil, fmt.Errorf("missing required key property: %s", keyProp)
		}
	}

	method = strings.ToUpper(method)
	if method != constants.PUT && method != constants.PATCH && method != constants.MERGE {
		return nil, fmt.Errorf("update method must be PUT, PATCH or MERGE, got %q", method)
	}

	// Convert numeric fields to strings for SAP OData v2 compatibility
	// This prevents "Failed to read property 'Quantity' at offset" errors
	updateData = utils.ConvertNumericsInMap(updateData)

	// Convert date fields to OData legacy format if needed
	if b.config.LegacyDates {
		updateData = utils.ConvertDatesInMap(updateData, false) // false = convert ISO to legacy
	}

	// Call OData client to update entity
	response, err := b.client.UpdateEntity(ctx, entitySetName, key, updateData, method)
	if err != nil {
		return nil, fmt.Errorf("failed to update entity: %w", err)
	}

	// Enhance response (includes date conversion if enabled)
	response = b.enhanceResponse(response, make(map[string]string))

	// Format response as JSON string
	result, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("failed to format response: %w", err)
	}

	return string(result), nil
}

func (b *ODataMCPBridge) handleEntityDelete(ctx context.Context, entitySetName string, entityType *models.EntityType, args map[string]any) (any, error) {
	// Build key values from arguments, wrapping GUIDs appropriately
	key := make(map[string]any)
	for _, keyProp := range entityType.KeyProperties {
		if value, exists := args[keyProp]; exists {
			propType := getPropertyType(entityType, keyProp)
			key[keyProp] = wrapKeyValueForType(value, propType)
		} else {
			return nil, fmt.Errorf("missing required key property: %s", keyProp)
		}
	}

	// Call OData client to delete entity
	_, err := b.client.DeleteEntity(ctx, entitySetName, key)
	if err != nil {
		return nil, fmt.Errorf("failed to delete entity: %w", err)
	}

	// For successful deletes, return a simple success message
	return `{"status": "success", "message": "Entity deleted successfully"}`, nil
}

func (b *ODataMCPBridge) handleFunctionCall(ctx context.Context, functionName string, function *models.FunctionImport, args map[string]any) (any, error) {
	// Build parameters from arguments
	parameters := make(map[string]any)
	for _, param := range function.Parameters {
		if param.Mode == "In" || param.Mode == "InOut" {
			if value, exists := args[param.Name]; exists {
				parameters[param.Name] = value
			} else if !param.Nullable {
				return nil, fmt.Errorf("missing required parameter: %s", param.Name)
			}
		}
	}

	// Determine HTTP method (default to GET if not specified)
	method := function.HTTPMethod
	if method == "" {
		method = constants.GET
	}

	var response *models.ODataResponse
	var err error
	if function.IsBound {
		target, bindErr := b.resolveBinding(function)
		if bindErr != nil {
			return nil, bindErr
		}
		key, keyErr := target.key(args)
		if keyErr != nil {
			return nil, keyErr
		}
		response, err = b.client.CallBoundFunction(ctx, target.entitySet, key, boundOperationName(functionName, function), parameters, method)
	} else {
		response, err = b.client.CallFunction(ctx, functionName, parameters, method)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to call function: %w", err)
	}

	// Format response as JSON string
	result, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("failed to format response: %w", err)
	}

	return string(result), nil
}

const (
	bindingKeyArg = "key"
	// bindingKeyArgFallback carries the key when the function declares its
	// own parameter called "key", which would otherwise be shadowed.
	bindingKeyArgFallback = "binding_key"
)

type binding struct {
	entitySet  string
	entityType *models.EntityType
	keyArg     string
}

func (b *ODataMCPBridge) resolveBinding(function *models.FunctionImport) (*binding, error) {
	typeName, isCollection := collectionElementType(function.BindingParameter.Type)
	entitySetName, entityType, err := b.bindingEntitySet(typeName)
	if err != nil {
		return nil, err
	}

	target := &binding{entitySet: entitySetName, entityType: entityType}
	if !isCollection {
		target.keyArg = bindingKeyArgFor(function)
	}

	return target, nil
}

func collectionElementType(typeName string) (string, bool) {
	if strings.HasPrefix(typeName, "Collection(") && strings.HasSuffix(typeName, ")") {
		return typeName[len("Collection(") : len(typeName)-1], true
	}

	return typeName, false
}

func bindingKeyArgFor(function *models.FunctionImport) string {
	for _, param := range function.Parameters {
		if param.Name == bindingKeyArg {
			return bindingKeyArgFallback
		}
	}

	return bindingKeyArg
}

func (target *binding) key(args map[string]any) (map[string]any, error) {
	if target.keyArg == "" {
		return nil, nil
	}

	keyArg, ok := args[target.keyArg].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing required parameter: %s (the %s entity to call it on)", target.keyArg, target.entitySet)
	}

	key := make(map[string]any, len(target.entityType.KeyProperties))
	for _, keyProp := range target.entityType.KeyProperties {
		value, exists := keyArg[keyProp]
		if !exists {
			return nil, fmt.Errorf("missing required key property: %s", keyProp)
		}
		key[keyProp] = wrapKeyValueForType(value, getPropertyType(target.entityType, keyProp))
	}

	return key, nil
}

// boundOperationName qualifies the name only outside the default container,
// where the protocol requires it; inside, the bare name is what servers expect.
func boundOperationName(functionName string, function *models.FunctionImport) string {
	if function.Container == "" {
		return functionName
	}

	return function.Container + "." + functionName
}

// bindingEntitySet finds the entity set to address a bound function through.
// Metadata only names the binding entity type, so a type exposed by several
// entity sets cannot be resolved without guessing.
func (b *ODataMCPBridge) bindingEntitySet(typeName string) (string, *models.EntityType, error) {
	entityType := b.lookupEntityType(typeName)
	if entityType == nil {
		return "", nil, fmt.Errorf("binding type %s not found in metadata", typeName)
	}

	var matches []string
	for name, entitySet := range b.metadata.EntitySets {
		if b.lookupEntityType(entitySet.EntityType) == entityType {
			matches = append(matches, name)
		}
	}

	if len(matches) != 1 {
		slices.Sort(matches)
		return "", nil, fmt.Errorf("binding type %s must belong to exactly one entity set, found %d %v", typeName, len(matches), matches)
	}

	return matches[0], entityType, nil
}
