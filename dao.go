package neatly

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/viant/toolbox"
	"github.com/viant/toolbox/data"
	"github.com/viant/toolbox/storage"
	"github.com/viant/toolbox/url"
	"path"
	"strings"
)

//OwnerURL represewnt currently loading netly URL
const OwnerURL = "ownerURL"

//Dao represents neatly data access object
type Dao struct {
	localResourceRepo  string
	remoteResourceRepo string
	factory            toolbox.DecoderFactory
	converter          *toolbox.Converter
}

//Load reads data from provided resource into the target pointer
func (d *Dao) Load(context data.Map, source *url.Resource, target interface{}) error {
	text, err := source.DownloadText()
	if err != nil {
		return err
	}
	d.AddStandardUdf(context)
	scanner := bufio.NewScanner(strings.NewReader(text))
	targetMap, err := d.load(context, source, scanner)
	if err != nil {
		return err
	}

	var sourceMap = make(map[string]interface{})

	err = d.converter.AssignConverted(&sourceMap, source);
	if err != nil {
		return err
	}
	targetMap["Source"] = sourceMap
	return d.converter.AssignConverted(target, targetMap)
}

//AddStandardUdf register building udf to the context
func (d *Dao) AddStandardUdf(context data.Map) {
	context.Put("AsMap", AsMap)
	context.Put("AsInt", AsInt)
	context.Put("AsFloat", AsFloat)
	context.Put("AsBool", AsBool)
	context.Put("HasResource", HasResource)
	context.Put("Md5", Md5)
}

//processTag creates a data structure in the result data.Map, it also check if the referenceValue for tag was Used before unless it is the first tag (result tag)
func (d *Dao) processTag(context *tagContext) (err error) {

	if context.objectContainer.Has(context.tag.Name) {
		return nil
	}
	if context.tag.IsArray {
		var collection = data.NewCollection()
		context.objectContainer.Put(context.tag.Name, collection)
		err = context.referenceValues.Apply(context.tag.Name, collection)
	} else {
		var object = make(map[string]interface{})
		context.objectContainer.Put(context.tag.Name, object)
		err = context.referenceValues.Apply(context.tag.Name, object)
	}
	return err
}

//processHeaderLine extract from LineNumber a tag from column[0], add deferredRefences for a tag, decodes fields from remaining column,
func (d *Dao) processHeaderLine(context *tagContext, decoder toolbox.Decoder, lineNumber int) (*toolbox.DelimiteredRecord, *Tag, error) {
	record := &toolbox.DelimiteredRecord{Delimiter: ","}
	err := decoder.Decode(record)
	if err != nil {
		return nil, nil, err
	}
	ownerName := context.rootObject.GetString("Name")
	context.tag = NewTag(ownerName, context.source, record.Columns[0], lineNumber)
	err = d.processTag(context)
	if err != nil {
		return nil, nil, err
	}
	return record, context.tag, nil
}

//processHeaderLine extract from LineNumber a tag from column[0], add deferredRefences for a tag, decodes fields from remaining column,
func (d *Dao) processRootHeaderLine(source *url.Resource, objectContainer data.Map, decoder toolbox.Decoder) (*toolbox.DelimiteredRecord, *Tag, error) {
	record := &toolbox.DelimiteredRecord{Delimiter: ","}
	err := decoder.Decode(record)
	if err != nil {
		return nil, nil, err
	}
	tag := NewTag("", source, record.Columns[0], 0)
	var object = make(map[string]interface{})
	objectContainer.Put(tag.Name, object)
	return record, tag, nil
}

//load loads source using nearly format.
func (d *Dao) load(loadingContext data.Map, source *url.Resource, scanner *bufio.Scanner) (map[string]interface{}, error) {
	var objectContainer = data.NewMap()
	var referenceValues = newReferenceValues()
	lines := readLines(scanner)
	decoder := d.factory.Create(strings.NewReader(lines[0]))
	record, tag, err := d.processRootHeaderLine(source, objectContainer, decoder)
	if err != nil {
		return nil, err
	}
	var rootObject = objectContainer.GetMap(tag.Name)
	var context = newTagContext(loadingContext, source, tag, objectContainer, referenceValues, rootObject, rootObject)
	for i := 1; i < len(lines); i++ {
		var recordHeight = 0
		line := lines[i]
		var hasActiveIterator = tag.HasActiveIterator()
		line = d.expandMeta(context, line)
		isHeaderLine := !strings.HasPrefix(line, ",")
		decoder := d.factory.Create(strings.NewReader(line))
		if isHeaderLine {
			if hasActiveIterator {
				if tag.Iterator.Next() {
					i = tag.LineNumber
					continue
				}
			}
			record, tag, err = d.processHeaderLine(context, decoder, i)
			if err != nil {
				return nil, err
			}
			continue
		}
		record.Record = make(map[string]interface{})
		err := decoder.Decode(record)
		if err != nil {
			return nil, err
		}

		if !record.IsEmpty() {
			tag.setTagObject(context, record.Record)

			for j := 1; j < len(record.Columns); j++ {
				if recordHeight, err = d.processCell(context,  record, lines, i, j, recordHeight, true); err != nil {
					return nil, err
				}
			}
			for j := 1; j < len(record.Columns); j++ {
				if recordHeight, err = d.processCell(context,  record, lines, i, j, recordHeight, false); err != nil {
					return nil, err
				}
			}
		}

		i += recordHeight
		var isLast = i+1 == len(lines)
		if isLast && tag.HasActiveIterator() {

			if tag.Iterator.Next() {
				i = tag.LineNumber
				continue
			}
		}
	}
	err = referenceValues.CheckUnused()
	if err != nil {
		return nil, err
	}
	return rootObject, nil
}

func (d *Dao) processCell(context *tagContext, record *toolbox.DelimiteredRecord, lines []string, recordIndex, columnIndex int, recordHeight int, virtual bool) (int, error) {
	fieldExpression := record.Columns[columnIndex]
	if fieldExpression == "" {
		return recordHeight, nil
	}
	field := NewField(fieldExpression)

	value, has := record.Record[field.expression]
	if !has || value == nil || toolbox.AsString(value) == "" {
		return recordHeight, nil
	}
	if (virtual && !field.IsVirtual) || (!virtual && field.IsVirtual) {
		return recordHeight, nil
	}


	tagObject := context.tagObject
	rootObject := context.rootObject
	textValue := toolbox.AsString(value)

	if strings.HasPrefix(textValue, "%%") { //escape forward object tag reference
		textValue = string(textValue[1:])
	} else {
		isReference := strings.HasPrefix(textValue, "%")
		if isReference {
			err := context.referenceValues.Add(string(textValue[1:]), field, tagObject)
			return recordHeight, err
		}
	}
	val, err := d.normalizeValue(context, textValue)
	if err != nil {
		return recordHeight, fmt.Errorf("%v - failed to normalizeValue %v, %v", context.tag.TagId(), textValue, err)
	}

	var targetObject data.Map
	if field.IsRoot {
		setRootField(field, rootObject, val)
		return recordHeight, nil
	}

	if field.IsVirtual {
		targetObject = context.virtualObjects
	} else {
		targetObject = tagObject
	}
	field.Set(val, targetObject)
	if field.HasArrayComponent {
		recordHeight, err = d.processArrayValues(context, field, recordIndex, lines, record, targetObject, recordHeight)
	}
	return recordHeight, err

}

func (d *Dao) processArrayValues(context *tagContext, field *Field, recordIndex int, lines []string, record *toolbox.DelimiteredRecord, data data.Map, recordHeight int) (int, error) {
	if field.HasArrayComponent {
		var itemCount = 0
		for k := recordIndex + 1; k < len(lines); k++ {
			if !strings.HasPrefix(lines[k], ",") {
				break
			}
			arrayValueDecoder := d.factory.Create(strings.NewReader(lines[k]))
			arrayItemRecord := &toolbox.DelimiteredRecord{
				Columns:   record.Columns,
				Delimiter: record.Delimiter,
			}
			err := arrayValueDecoder.Decode(arrayItemRecord)
			if err != nil {
				return 0, err
			}
			itemValue := arrayItemRecord.Record[field.expression]
			if itemValue == nil || toolbox.AsString(itemValue) == "" {
				break
			}
			itemCount++
			val, err := d.normalizeValue(context, toolbox.AsString(itemValue))
			if err != nil {
				return 0, err
			}
			field.Set(val, data, itemCount)
		}
		if recordHeight < itemCount {
			recordHeight = itemCount
		}
	}
	return recordHeight, nil
}

func setRootField(field *Field, rootObject data.Map, val interface{}) {
	if field.HasArrayComponent {
		var expr = strings.Replace(field.expression, "[]", "", 1)
		expr = strings.Replace(expr, "/", "", 1)

		bucket, has := rootObject.GetValue(expr)
		if !has {
			bucket = data.NewCollection()
		}
		var bucketSlice = toolbox.AsSlice(bucket)
		if toolbox.IsSlice(val) {
			aSlice := toolbox.AsSlice(val)
			for _, item := range aSlice {
				bucketSlice = append(bucketSlice, item)
			}
		} else {
			bucketSlice = append(bucketSlice, val)
		}
		rootObject.SetValue(expr, bucketSlice)

	} else {
		field.Set(val, rootObject)
	}
}

func readLines(scanner *bufio.Scanner) []string {
	var lines = make([]string, 0)

	for scanner.Scan() {
		var line = scanner.Text()
		if len(lines) == 0 && strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "//") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

/*
getExternalResource returns resource for provided asset URI. This function tries to load asset using the following methods:

1) For valid URL :  new resource if returned with owner resource credential
2) For asset starting  with / new file resource if returned with owner resource credential
3) For asset starting with # has is being stripped out and asset is being loaded relative path asset
4) For asset with relative path the following lookup are being Used, the first successful creates new resource with owner resource credential
	a) owner resource path with subpath if provided and  asset name
	b) owner resource path  without subpath and asset name
	c) Local/remoteResourceRepo and asset name

*/
func (d *Dao) getExternalResource(context *tagContext, URI string) (*url.Resource, error) {
	if URI == "" {
		return nil, fmt.Errorf("Resource was empty")
	}
	if strings.Contains(URI, "://") || strings.HasPrefix(URI, "/") {
		return url.NewResource(URI, context.source.Credential), nil
	}
	if strings.HasPrefix(URI, "#") {
		URI = string(URI[1:])
	}

	ownerURL, URL := buildURLWithOwnerURL(context.source, context.tag.Subpath, URI)

	service, err := storage.NewServiceForURL(URL, context.source.Credential)
	if err != nil {
		return nil, err
	}
	exists, err := service.Exists(URL)
	if !exists {
		if d.remoteResourceRepo != "" {
			fallbackResource, err := d.NewRepoResource(context.context, URI)
			if err == nil {
				service, _ = storage.NewServiceForURL(fallbackResource.URL, context.source.Credential)
				if exists, _ = service.Exists(fallbackResource.URL); exists {
					URL = fallbackResource.URL

				}
			}
		}
		if !exists && context.tag.Subpath != "" {
			fileCandidate := path.Join(ownerURL,  context.tag.Subpath, URI)
			URL = toolbox.FileSchema + fileCandidate
		}
	}
	return url.NewResource(URL, context.source.Credential), nil
}

//buildURLWithOwnerURL builds owner URL and candidate URL based on owner url, subpath if not empty, and URI
func buildURLWithOwnerURL(owner *url.Resource, subpath string, URI string) (string, string) {
	var URL string
	ownerURL, _ := toolbox.URLSplit(owner.URL)

	if subpath != "" {
		fileCandidate := toolbox.URLPathJoin(ownerURL, path.Join(subpath, URI))
		fileCandidate = strings.Replace(fileCandidate, toolbox.FileSchema, "", 1)
		if toolbox.FileExists(fileCandidate) {
			URL = toolbox.FileSchema + fileCandidate
		}
	}
	if URL == "" {
		URL = toolbox.URLPathJoin(ownerURL, URI)
	}
	return ownerURL, URL
}

/*
NewRepoResource returns resource build as localResourceURL/remoteResourceURL and URI
If Local resource does not exist but remote does it copy it over to Local to avoid remote round trips in the future.
*/
func (d *Dao) NewRepoResource(context data.Map, URI string) (*url.Resource, error) {
	var localResourceURL = fmt.Sprintf(d.localResourceRepo, URI)
	var localResource = url.NewResource(localResourceURL)

	if toolbox.FileExists(localResource.ParsedURL.Path) {
		return url.NewResource(localResourceURL), nil
	}
	var localService, err = storage.NewServiceForURL(localResourceURL, "")
	if err != nil {
		return nil, err
	}
	var remoteResourceURL = fmt.Sprintf(d.remoteResourceRepo, URI)
	remoteService, err := storage.NewServiceForURL(remoteResourceURL, "")
	if err != nil {
		return nil, err
	}
	err = storage.Copy(remoteService, remoteResourceURL, localService, localResourceURL, nil)
	return localResource, err
}

//asJSONText converts source into json string
func asJSONText(source interface{}) string {
	if source == nil {
		return ""
	}
	var buf = new(bytes.Buffer)
	err := toolbox.NewJSONEncoderFactory().Create(buf).Encode(source)
	if err == nil {
		return buf.String()
	}
	return toolbox.AsString(source)
}

/*
loadMap loads map for provided URI. If resource is a json or yaml object it will be converted into a map[string]interface{}
index parameters publishes $arg{index} or $args{index} additional key value pairs, the fist one has full content of the resource, the latter
has removed the first and last character. This is to provide ability to substiture with entire json object including {} or just content of the json object.
*/
func (d *Dao) loadMap(context *tagContext, asset string, escapeQuotes bool, index int) (data.Map, error) {
	virtualObjects := context.virtualObjects
	var aMap = make(map[string]interface{})
	var uriExtension string
	var assetContent = asset

	if strings.HasPrefix(strings.TrimSpace(asset), "$") {
		asset = strings.TrimSpace(asset)
		value, has := virtualObjects.GetValue(string(asset[1:]))
		if !has {
			return nil, fmt.Errorf("Failed resolve $%v as variable substitution source", asset)
		}
		if toolbox.IsSlice(value) || toolbox.IsMap(value) {
			assetContent = asJSONText(value)
		} else {
			assetContent = toolbox.AsString(value)
		}
	} else if strings.HasPrefix(asset, "#") {
		uriExtension = path.Ext(asset)
		resource, err := d.getExternalResource(context, asset)
		if err != nil {
			return nil, err
		}
		assetContent, err = resource.DownloadText()
		if err != nil {
			return nil, err
		}
	}

	assetContent = d.expandMeta(context, assetContent)
	assetContent = strings.Trim(assetContent, " \t\n\r")

	if uriExtension == ".yaml" || uriExtension == ".yml" {
		err := toolbox.NewYamlDecoderFactory().Create(strings.NewReader(assetContent)).Decode(&aMap)
		if err != nil {
			return nil, err
		}
	} else if strings.HasPrefix(assetContent, "{") {
		err := toolbox.NewJSONDecoderFactory().Create(strings.NewReader(assetContent)).Decode(&aMap)
		if err != nil {
			assetContentLength := len(assetContent)
			if assetContentLength > 50 {
				assetContentLength = 50
			}
			return nil, fmt.Errorf("Failed to decode json:%v, %v", string(assetContent[:assetContentLength]), err)
		}
	}
	if escapeQuotes {
		for k, v := range aMap {
			if v == nil {
				continue
			}
			if toolbox.IsMap(v) || toolbox.IsSlice(v) {
				buf := new(bytes.Buffer)
				err := toolbox.NewJSONEncoderFactory().Create(buf).Encode(v)
				if err != nil {
					return nil, err
				}
				v = buf.String()
			}
			if toolbox.IsString(v) {
				textValue := toolbox.AsString(v)
				if strings.Contains(textValue, "\"") {
					textValue = strings.Replace(textValue, "\\", "\\\\", len(textValue))
					textValue = strings.Replace(textValue, "\n", "", len(textValue))
					textValue = strings.Replace(textValue, "\"", "\\\"", len(textValue))
					aMap[k] = textValue

				}
			}
		}
	}
	aMap[fmt.Sprintf("arg%v", index)] = assetContent
	aMap[fmt.Sprintf("args%v", index)] = string(assetContent[1: len(assetContent)-1])
	return data.Map(aMap), nil
}

func (d *Dao) loadExternalResource(context *tagContext,  assetURI string) (string, error) {
	resource, err := d.getExternalResource(context, assetURI)
	var result string
	if err == nil {
		result, err = resource.DownloadText()
	}
	if err != nil {
		return "", fmt.Errorf("Failed to load external resource: %v %v", assetURI, err)
	}
	return result, err
}

func (d *Dao) asDataStructure(value string) (interface{}, error) {
	if len(value) == 0 {
		return nil, nil
	}
	if strings.HasPrefix(value, "{{") || strings.HasSuffix(value, "}}") {
		return string(value[1: len(value)-1]), nil
	}

	if strings.HasPrefix(value, "[[") || strings.HasSuffix(value, "]]") {
		return string(value[1: len(value)-1]), nil
	}

	if strings.HasPrefix(value, "{") {
		var jsonObject = make(map[string]interface{})
		err := toolbox.NewJSONDecoderFactory().Create(strings.NewReader(value)).Decode(&jsonObject)
		if err != nil {
			return nil, fmt.Errorf("Failed to decode: %v %T, %v", value, value, err)
		}
		return jsonObject, nil
	} else if strings.HasPrefix(value, "[") {
		var jsonArray = make([]interface{}, 0)
		err := toolbox.NewJSONDecoderFactory().Create(strings.NewReader(value)).Decode(&jsonArray)
		if err != nil {
			return nil, fmt.Errorf("Failed to decode: %v %v", value, err)
		}
		return jsonArray, nil
	}
	return value, nil
}

func (d *Dao) expandMeta(context *tagContext, text string) string {
	var replacementMap = data.NewMap()
	replacementMap.Put("tagId", context.tag.TagId())
	replacementMap.Put("tag", context.tag.Name)
	if context.tag.HasActiveIterator() {
		replacementMap.Put("index", context.tag.Iterator.Index())
	}
	return replacementMap.ExpandAsText(text)
}

func (d *Dao) normalizeValue(context *tagContext, value string) (interface{}, error) {
	virtualObjects := context.virtualObjects
	if strings.HasPrefix(value, "##") { //escape #
		value = string(value[1:])
	} else if strings.HasPrefix(value, "#") {
		context.context.Put(OwnerURL, context.source.URL)

		var assets = strings.Split(value, "|")
		mainAsset, err := d.loadExternalResource(context, assets[0])
		if err != nil {
			return nil, err
		}
		mainAsset = strings.TrimSpace(mainAsset)
		mainAsset = d.expandMeta(context, mainAsset)
		escapeQuotes := strings.HasPrefix(mainAsset, "{") || strings.HasPrefix(mainAsset, "[")
		for i := 1; i < len(assets); i++ {
			aMap, err := d.loadMap(context, assets[i], escapeQuotes, i-1)
			if err != nil {
				return nil, err
			}
			mainAsset = aMap.ExpandAsText(mainAsset)
		}
		value = mainAsset
	}

	result, err := d.asDataStructure(value)
	if err == nil {
		result = context.context.Expand(result)
		if len(virtualObjects) > 0 {
			result = virtualObjects.Expand(result)
		}
	}
	return result, err
}

//NewDao creates a new neatly format compatibile format data access object.
//It takes localResourceRepo, remoteResourceRepo, dataFormat and optionally delimiterDecoderFactory
func NewDao(localResourceRepo, remoteResourceRepo, dataFormat string, delimiterDecoderFactory toolbox.DecoderFactory) *Dao {
	if delimiterDecoderFactory == nil {
		delimiterDecoderFactory = toolbox.NewDelimiterDecoderFactory()
	}
	return &Dao{
		localResourceRepo:  localResourceRepo,
		remoteResourceRepo: remoteResourceRepo,
		factory:            delimiterDecoderFactory,
		converter:          toolbox.NewColumnConverter(toolbox.DateFormatToLayout(dataFormat)),
	}
}

type tagContext struct {
	source          *url.Resource
	context         data.Map
	referenceValues referenceValues
	objectContainer data.Map
	rootObject      data.Map
	tag             *Tag
	tagObject       data.Map
	virtualObjects  data.Map
}

func newTagContext(context data.Map, source *url.Resource, tag *Tag, objectContainer data.Map, referenceValues referenceValues, rootObject data.Map, tagObject data.Map) *tagContext {
	return &tagContext{
		source:          source,
		context:         context,
		tag:             tag,
		objectContainer: objectContainer,
		referenceValues: referenceValues,
		rootObject:      rootObject,
		tagObject:       tagObject,
		virtualObjects:  data.NewMap(),
	}
}
