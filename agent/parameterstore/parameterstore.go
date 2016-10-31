// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package parameterstore contains modules to resolve ssm parameters present in the document.
package parameterstore

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/aws/amazon-ssm-agent/agent/sdkutil"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
)

const (
	// defaultParamName is used for creating default regex for parameter name
	defaultParamName = ""

	// ParamTypeString represents the Param Type is SecureString
	ParamTypeSecureString = "SecureString"

	// ParamTypeStringList represents the Param Type is StringList
	ParamTypeStringList = "StringList"
)

var callParameterService = callGetParameters

// Resolve resolves ssm parameters of the format {{ssm:*}}
func Resolve(log log.T, input interface{}, resolveSecureString bool) (interface{}, error) {
	validSSMParam, err := getValidSSMParamRegexCompiler(log, defaultParamName)
	if err != nil {
		return input, err
	}

	// Extract all SSM parameters from input
	ssmParams := extractSSMParameters(log, input, validSSMParam)

	// Return original string if no ssm params found
	if len(ssmParams) == 0 {
		return input, nil
	}

	// Get ssm parameter values
	resolvedParamMap, err := getSSMParameterValues(log, ssmParams, resolveSecureString)
	if err != nil {
		return input, err
	}

	// Replace ssm parameter names with their values
	input = replaceSSMParameters(log, input, resolvedParamMap)

	// Return resolved input
	return input, nil
}

// ResolveSecureString resolves the ssm parameters if present in input string
func ResolveSecureString(log log.T, input string) (string, error) {
	output, err := Resolve(log, input, true)
	if err != nil {
		return input, err
	}

	var reformatOutput string
	err = jsonutil.Remarshal(output, &reformatOutput)
	if err != nil {
		return input, err
	}

	return reformatOutput, nil
}

// ResolveSecureStringForStringList resolves the ssm parameters if present in input stringList
func ResolveSecureStringForStringList(log log.T, input []string) ([]string, error) {
	output, err := Resolve(log, input, true)
	if err != nil {
		return input, err
	}

	var reformatOutput []string
	err = jsonutil.Remarshal(output, &reformatOutput)
	if err != nil {
		return input, err
	}

	return reformatOutput, nil
}

// getValidSSMParamRegexCompiler returns a regex compiler
func getValidSSMParamRegexCompiler(log log.T, paramName string) (*regexp.Regexp, error) {
	var validSSMParamRegex string

	if strings.Compare(paramName, defaultParamName) == 0 {
		validSSMParamRegex = "\\{\\{ *ssm:([/\\w]+) *}}"
	} else {
		validSSMParamRegex = "\\{\\{ *ssm:" + paramName + " *}}"
	}

	validSSMParam, err := regexp.Compile(validSSMParamRegex)
	if err != nil {
		errorString := fmt.Errorf("Invalid regular expression used to resolve ssm parameters. Error: %v", err)
		log.Debug(errorString)
		return nil, errorString
	}
	return validSSMParam, nil
}

// getSSMParameterValues takes a list of strings and resolves them by calling the GetParameters API
func getSSMParameterValues(log log.T, ssmParams []string, resolveSecureString bool) (map[string]Parameter, error) {
	var result *GetParametersResponse
	var err error

	log.Info("Resolving SSM parameters")

	validParamRegex := ":([/\\w]+)*"
	validParam, err := regexp.Compile(validParamRegex)
	if err != nil {
		errorString := fmt.Errorf("Invalid regular expression used to resolve ssm parameters. Error: %v", err)
		log.Debug(errorString)
		return nil, errorString
	}

	// Remove duplicates
	paramNames := []string{}
	seen := map[string]bool{}
	for _, value := range ssmParams {
		temp := validParam.FindString(value)
		temp = temp[1:]
		if !seen[temp] {
			seen[temp] = true
			paramNames = append(paramNames, temp)
		}
	}

	if result, err = callParameterService(log, paramNames); err != nil {
		return nil, err
	}

	if len(paramNames) != len(result.Parameters) {
		errorString := fmt.Errorf("Input contains invalid ssm parameters %v", result.InvalidParameters)
		log.Debug(errorString)
		return nil, errorString
	}

	resolvedParamMap := map[string]Parameter{}
	for _, paramObj := range result.Parameters {
		// Skip secure parameters
		if !resolveSecureString && strings.Compare(paramObj.Type, ParamTypeSecureString) == 0 {
			continue
		}

		// get regex compiler
		validSSMParam, err := getValidSSMParamRegexCompiler(log, paramObj.Name)
		if err != nil {
			return nil, err
		}

		for _, value := range ssmParams {
			if validSSMParam.MatchString(value) {
				resolvedParamMap[value] = paramObj
			}
		}
	}

	return resolvedParamMap, nil
}

// callGetParameters makes a GetParameters API call to the service
func callGetParameters(log log.T, paramNames []string) (*GetParametersResponse, error) {
	var result *ssm.GetParametersOutput

	serviceParams := &ssm.GetParametersInput{
		Names:          aws.StringSlice(paramNames),
		WithDecryption: aws.Bool(true),
	}

	log.Debugf("Calling GetParameters API with params - %v", serviceParams)

	// reading agent appconfig
	appCfg, err := appconfig.Config(false)
	if err != nil {
		log.Errorf("Could not load config file %v", err)
		return nil, err
	}

	// setting ssm client config
	cfg := sdkutil.AwsConfig()
	cfg.Region = &appCfg.Agent.Region
	cfg.Endpoint = &appCfg.Ssm.Endpoint

	ssmObj := ssm.New(session.New(cfg))

	if result, err = ssmObj.GetParameters(serviceParams); err != nil {
		log.Errorf("Encountered error while calling GetParameters API. Error: %v", err)
		return nil, err
	}

	var response GetParametersResponse
	err = jsonutil.Remarshal(result, &response)
	if err != nil {
		log.Errorf("Invalid format of GetParameters output. Error: %v", err)
		return nil, err
	}

	return &response, nil
}
