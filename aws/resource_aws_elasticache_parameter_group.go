package aws

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/elasticache"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/terraform-providers/terraform-provider-aws/aws/internal/hashcode"
)

func resourceAwsElasticacheParameterGroup() *schema.Resource {
	return &schema.Resource{
		Create: resourceAwsElasticacheParameterGroupCreate,
		Read:   resourceAwsElasticacheParameterGroupRead,
		Update: resourceAwsElasticacheParameterGroupUpdate,
		Delete: resourceAwsElasticacheParameterGroupDelete,
		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},
		Schema: map[string]*schema.Schema{
			"name": {
				Type:     schema.TypeString,
				ForceNew: true,
				Required: true,
				StateFunc: func(val interface{}) string {
					return strings.ToLower(val.(string))
				},
			},
			"family": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"description": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
				Default:  "Managed by Terraform",
			},
			"parameter": {
				Type:     schema.TypeSet,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Type:     schema.TypeString,
							Required: true,
						},
						"value": {
							Type:     schema.TypeString,
							Required: true,
						},
					},
				},
				Set: resourceAwsElasticacheParameterHash,
			},
		},
	}
}

func resourceAwsElasticacheParameterGroupCreate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).elasticacheconn

	createOpts := elasticache.CreateCacheParameterGroupInput{
		CacheParameterGroupName:   aws.String(d.Get("name").(string)),
		CacheParameterGroupFamily: aws.String(d.Get("family").(string)),
		Description:               aws.String(d.Get("description").(string)),
	}

	log.Printf("[DEBUG] Create ElastiCache Parameter Group: %#v", createOpts)
	resp, err := conn.CreateCacheParameterGroup(&createOpts)
	if err != nil {
		return fmt.Errorf("error creating ElastiCache Parameter Group: %w", err)
	}

	d.SetId(aws.StringValue(resp.CacheParameterGroup.CacheParameterGroupName))
	log.Printf("[INFO] ElastiCache Parameter Group ID: %s", d.Id())

	return resourceAwsElasticacheParameterGroupUpdate(d, meta)
}

func resourceAwsElasticacheParameterGroupRead(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).elasticacheconn

	describeOpts := elasticache.DescribeCacheParameterGroupsInput{
		CacheParameterGroupName: aws.String(d.Id()),
	}

	describeResp, err := conn.DescribeCacheParameterGroups(&describeOpts)
	if err != nil {
		return err
	}

	if len(describeResp.CacheParameterGroups) != 1 ||
		aws.StringValue(describeResp.CacheParameterGroups[0].CacheParameterGroupName) != d.Id() {
		return fmt.Errorf("unable to find Parameter Group: %#v", describeResp.CacheParameterGroups)
	}

	d.Set("name", describeResp.CacheParameterGroups[0].CacheParameterGroupName)
	d.Set("family", describeResp.CacheParameterGroups[0].CacheParameterGroupFamily)
	d.Set("description", describeResp.CacheParameterGroups[0].Description)

	// Only include user customized parameters as there's hundreds of system/default ones
	describeParametersOpts := elasticache.DescribeCacheParametersInput{
		CacheParameterGroupName: aws.String(d.Id()),
		Source:                  aws.String("user"),
	}

	describeParametersResp, err := conn.DescribeCacheParameters(&describeParametersOpts)
	if err != nil {
		return err
	}

	d.Set("parameter", flattenElastiCacheParameters(describeParametersResp.Parameters))

	return nil
}

func resourceAwsElasticacheParameterGroupUpdate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).elasticacheconn

	if d.HasChange("parameter") {
		o, n := d.GetChange("parameter")
		if o == nil {
			o = new(schema.Set)
		}
		if n == nil {
			n = new(schema.Set)
		}

		os := o.(*schema.Set)
		ns := n.(*schema.Set)

		toRemove := expandElastiCacheParameters(os.Difference(ns).List())

		log.Printf("[DEBUG] Parameters to remove: %#v", toRemove)

		toAdd := expandElastiCacheParameters(ns.Difference(os).List())

		log.Printf("[DEBUG] Parameters to add: %#v", toAdd)

		// We can only modify 20 parameters at a time, so walk them until
		// we've got them all.
		maxParams := 20

		for len(toRemove) > 0 {
			var paramsToModify []*elasticache.ParameterNameValue
			if len(toRemove) <= maxParams {
				paramsToModify, toRemove = toRemove[:], nil
			} else {
				paramsToModify, toRemove = toRemove[:maxParams], toRemove[maxParams:]
			}
			resetOpts := elasticache.ResetCacheParameterGroupInput{
				CacheParameterGroupName: aws.String(d.Get("name").(string)),
				ParameterNameValues:     paramsToModify,
			}

			log.Printf("[DEBUG] Reset ElastiCache Parameter Group: %s", resetOpts)
			err := resource.Retry(30*time.Second, func() *resource.RetryError {
				_, err := conn.ResetCacheParameterGroup(&resetOpts)
				if err != nil {
					if isAWSErr(err, "InvalidCacheParameterGroupState", " has pending changes") {
						return resource.RetryableError(err)
					}
					return resource.NonRetryableError(err)
				}
				return nil
			})

			// When attempting to reset the reserved-memory parameter, the API
			// can return the below 500 error, which causes the AWS Go SDK to
			// automatically retry and hence timeout resource.Retry():
			//   InternalFailure: An internal error has occurred. Please try your query again at a later time.
			// Instead of hardcoding the reserved-memory parameter removal
			// above, which may become out of date, here we add logic to
			// workaround this API behavior

			if isResourceTimeoutError(err) {
				for i, paramToModify := range paramsToModify {
					if aws.StringValue(paramToModify.ParameterName) != "reserved-memory" {
						continue
					}

					// Always reset the top level error and remove the reset for reserved-memory
					err = nil
					paramsToModify = append(paramsToModify[:i], paramsToModify[i+1:]...)

					// If we are only trying to remove reserved-memory and not perform
					// an update to reserved-memory or reserved-memory-percentage, we
					// can attempt to workaround the API issue by switching it to
					// reserved-memory-percentage first then reset that temporary parameter.

					tryReservedMemoryPercentageWorkaround := true

					allConfiguredParameters := expandElastiCacheParameters(d.Get("parameter").(*schema.Set).List())
					if err != nil {
						return fmt.Errorf("error expanding parameter configuration: %w", err)
					}

					for _, configuredParameter := range allConfiguredParameters {
						if aws.StringValue(configuredParameter.ParameterName) == "reserved-memory" || aws.StringValue(configuredParameter.ParameterName) == "reserved-memory-percentage" {
							tryReservedMemoryPercentageWorkaround = false
							break
						}
					}

					if !tryReservedMemoryPercentageWorkaround {
						break
					}

					// The reserved-memory-percentage parameter does not exist in redis2.6 and redis2.8
					family := d.Get("family").(string)
					if family == "redis2.6" || family == "redis2.8" {
						log.Printf("[WARN] Cannot reset ElastiCache Parameter Group (%s) reserved-memory parameter with %s family", d.Id(), family)
						break
					}

					modifyInput := &elasticache.ModifyCacheParameterGroupInput{
						CacheParameterGroupName: aws.String(d.Get("name").(string)),
						ParameterNameValues: []*elasticache.ParameterNameValue{
							{
								ParameterName:  aws.String("reserved-memory-percentage"),
								ParameterValue: aws.String("0"),
							},
						},
					}
					_, err = conn.ModifyCacheParameterGroup(modifyInput)

					if err != nil {
						log.Printf("[WARN] Error attempting reserved-memory workaround to switch to reserved-memory-percentage: %s", err)
						break
					}

					resetInput := &elasticache.ResetCacheParameterGroupInput{
						CacheParameterGroupName: aws.String(d.Get("name").(string)),
						ParameterNameValues: []*elasticache.ParameterNameValue{
							{
								ParameterName:  aws.String("reserved-memory-percentage"),
								ParameterValue: aws.String("0"),
							},
						},
					}

					_, err = conn.ResetCacheParameterGroup(resetInput)

					if err != nil {
						log.Printf("[WARN] Error attempting reserved-memory workaround to reset reserved-memory-percentage: %s", err)
					}

					break
				}

				// Retry any remaining parameter resets with reserved-memory potentially removed
				if len(paramsToModify) > 0 {
					resetOpts = elasticache.ResetCacheParameterGroupInput{
						CacheParameterGroupName: aws.String(d.Get("name").(string)),
						ParameterNameValues:     paramsToModify,
					}
					// Reset top level error with potentially any new errors
					_, err = conn.ResetCacheParameterGroup(&resetOpts)
				}
			}

			if err != nil {
				return fmt.Errorf("error resetting ElastiCache Parameter Group: %w", err)
			}
		}

		for len(toAdd) > 0 {
			var paramsToModify []*elasticache.ParameterNameValue
			if len(toAdd) <= maxParams {
				paramsToModify, toAdd = toAdd[:], nil
			} else {
				paramsToModify, toAdd = toAdd[:maxParams], toAdd[maxParams:]
			}
			modifyOpts := elasticache.ModifyCacheParameterGroupInput{
				CacheParameterGroupName: aws.String(d.Get("name").(string)),
				ParameterNameValues:     paramsToModify,
			}

			log.Printf("[DEBUG] Modify ElastiCache Parameter Group: %s", modifyOpts)
			_, err := conn.ModifyCacheParameterGroup(&modifyOpts)
			if err != nil {
				return fmt.Errorf("error modifying ElastiCache Parameter Group: %w", err)
			}
		}
	}

	return resourceAwsElasticacheParameterGroupRead(d, meta)
}

func resourceAwsElasticacheParameterGroupDelete(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).elasticacheconn

	deleteOpts := elasticache.DeleteCacheParameterGroupInput{
		CacheParameterGroupName: aws.String(d.Id()),
	}
	err := resource.Retry(3*time.Minute, func() *resource.RetryError {
		_, err := conn.DeleteCacheParameterGroup(&deleteOpts)
		if err != nil {
			awsErr, ok := err.(awserr.Error)
			if ok && awsErr.Code() == "CacheParameterGroupNotFoundFault" {
				return nil
			}
			if ok && awsErr.Code() == "InvalidCacheParameterGroupState" {
				return resource.RetryableError(err)
			}
			return resource.NonRetryableError(err)
		}
		return nil
	})
	if isResourceTimeoutError(err) {
		_, err = conn.DeleteCacheParameterGroup(&deleteOpts)
	}
	if isAWSErr(err, elasticache.ErrCodeCacheParameterGroupNotFoundFault, "") {
		return nil
	}

	if err != nil {
		return fmt.Errorf("error deleting ElastiCache Parameter Group (%s): %w", d.Id(), err)
	}

	return nil
}

func resourceAwsElasticacheParameterHash(v interface{}) int {
	var buf bytes.Buffer
	m := v.(map[string]interface{})
	buf.WriteString(fmt.Sprintf("%s-", m["name"].(string)))
	buf.WriteString(fmt.Sprintf("%s-", m["value"].(string)))

	return hashcode.String(buf.String())
}

// Flattens an array of Parameters into a []map[string]interface{}
func flattenElastiCacheParameters(list []*elasticache.Parameter) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(list))
	for _, i := range list {
		if i.ParameterValue != nil {
			result = append(result, map[string]interface{}{
				"name":  strings.ToLower(aws.StringValue(i.ParameterName)),
				"value": aws.StringValue(i.ParameterValue),
			})
		}
	}
	return result
}

// Takes the result of flatmap.Expand for an array of parameters and
// returns Parameter API compatible objects
func expandElastiCacheParameters(configured []interface{}) []*elasticache.ParameterNameValue {
	parameters := make([]*elasticache.ParameterNameValue, len(configured))

	// Loop over our configured parameters and create
	// an array of aws-sdk-go compatible objects
	for i, pRaw := range configured {
		parameters[i] = expandElastiCacheParameter(pRaw.(map[string]interface{}))
	}

	return parameters
}

func expandElastiCacheParameter(param map[string]interface{}) *elasticache.ParameterNameValue {
	return &elasticache.ParameterNameValue{
		ParameterName:  aws.String(param["name"].(string)),
		ParameterValue: aws.String(param["value"].(string)),
	}
}
