package aws

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfTypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmTypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/charmbracelet/log"
	"github.com/schidstorm/wg-ondemand/pkg/provision"
)

var buildArgCustomQualifier string = "" // injected at build time
var bootstrapStackName string = "wg-ondemand-bootstrap"

func init() {
	customQualifier := os.Getenv("CDK_CUSTOM_QUALIFIER")
	if customQualifier != "" {
		buildArgCustomQualifier = customQualifier
	}
}

//go:embed cdk.template.yaml
var cdkTemplate string

//go:embed bootstrap.template.yaml
var bootstrapTemplate string

type AwsProvisioner struct {
	cfClient  *cloudformation.Client
	ssmClient *ssm.Client
	stsClient *sts.Client
	s3Client  *s3.Client
	ec2Client *ec2.Client
}

type AwsError interface {
	Service() string
	Operation() string
	Code() string
}

func (p *AwsProvisioner) Provision(ctx context.Context, id string, args provision.ProvisionArguments) (provision.ProvisionResult, error) {
	log.Info("Initialize SDK clients", "region", args.Region)
	err := p.initSdkClients(ctx, args.Region)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	var wgPort = strconv.Itoa(int(args.WgPort))

	log.Info("Provisioning bootstrap stack", "stackName", bootstrapStackName)
	_, _, err = p.provisionStack(ctx, bootstrapStackName, bootstrapTemplate, map[string]string{})
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	cdkSim := CdkSim{}
	cdkSim.Simulate(ctx, p.stsClient)

	log.Info("Provisioning stack", "stackName", id)
	stackOutput, stackRemoveHandler, err := p.provisionStack(ctx, id, cdkTemplate, map[string]string{
		"WgPort": wgPort,
	})
	if err != nil {
		return provision.ProvisionResult{}, err
	}
	removeHandler := func() {
		log.Info("Cleaning up stack", "stackName", id)
		stackRemoveHandler()
	}

	instanceId := stackOutput["InstanceId"]
	log.Info("Waiting for instance to be up", "instanceId", instanceId)
	err = p.waitUntilUp(ctx, instanceId)
	if err != nil {
		removeHandler()
		return provision.ProvisionResult{}, err
	}

	log.Info("Running init script")
	outputParams, err := args.RunInitScript(ctx, func(script string) (string, error) {
		stdout, stderr, err := p.runShell(ctx, instanceId, script)
		if err != nil {
			log.Error("Failed to run init script", "err", err, "stdout", stdout, "stderr", stderr)
		}
		return stdout, nil
	})
	if err != nil {
		removeHandler()
		return provision.ProvisionResult{}, err
	}

	return provision.ProvisionResult{
		ServerIP:        net.ParseIP(stackOutput["ServerIp"]),
		ServerWgIp:      args.ServerWgIp,
		ServerPublicKey: string(outputParams.ServerWgPublicKey),
	}, nil
}

func (p *AwsProvisioner) DeProvision(ctx context.Context, id string, args provision.DeProvisionArguments) error {
	err := p.initSdkClients(ctx, args.Region)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	numThreads := 3
	wg.Add(numThreads)
	errorsChannel := make(chan error, numThreads)

	go func() {
		defer wg.Done()
		identity, err := p.stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			errorsChannel <- err
			return
		}

		bucketName := fmt.Sprintf("cdk-%s-assets-%s-%s", buildArgCustomQualifier, *identity.Account, args.Region)
		errorsChannel <- retry(func() error {
			return p.deleteBucket(ctx, bucketName)
		})
	}()

	go func() {
		defer wg.Done()
		errorsChannel <- retry(func() error {
			return p.deleteStack(ctx, bootstrapStackName)
		})
	}()

	go func() {
		defer wg.Done()
		errorsChannel <- retry(func() error {
			return p.deleteStack(ctx, id)
		})
	}()

	errs := make([]error, 0)
	errsCollectDone := make(chan struct{})
	go func() {
		for err := range errorsChannel {
			if err != nil {
				errs = append(errs, err)
			}
		}

		close(errsCollectDone)
	}()

	wg.Wait()
	close(errorsChannel)
	<-errsCollectDone

	return errors.Join(errs...)
}

func retry(f func() error) error {
	var lastError error
	for retries := 20; retries > 0; retries-- {
		err := f()
		if err == nil {
			return nil
		}

		if errors.Is(err, context.Canceled) {
			return err
		} else {
			lastError = err
		}

		time.Sleep(1 * time.Second)
	}

	return lastError
}

func (p *AwsProvisioner) provisionStack(ctx context.Context, stackName, templateBody string, params map[string]string) (map[string]string, func(), error) {
	removeHandler := func() {
	}

	var cdkParameterList []cfTypes.Parameter
	for k, v := range params {
		cdkParameterList = append(cdkParameterList, cfTypes.Parameter{
			ParameterKey:   pstr(k),
			ParameterValue: pstr(v),
		})
	}

	_, err := p.cfClient.CreateStack(ctx, &cloudformation.CreateStackInput{
		StackName:    pstr(stackName),
		TemplateBody: pstr(templateBody),
		Capabilities: []cfTypes.Capability{
			cfTypes.CapabilityCapabilityNamedIam,
		},
		Parameters: cdkParameterList,
	})
	if err != nil {
		if !strings.Contains(err.Error(), "AlreadyExistsException") {
			return nil, removeHandler, err
		}
	}

	removeHandler = func() {
		_, err := p.cfClient.DeleteStack(ctx, &cloudformation.DeleteStackInput{
			StackName: pstr(stackName),
		})
		if err != nil {
			log.Error("Failed to delete stack", "err", err)
		}
	}

	// wait for stack to be created
	log.Debug("Waiting for stack to be created", "stackName", stackName)
	for {
		time.Sleep(10 * time.Second)
		resp, err := p.cfClient.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
			StackName: pstr(stackName),
		})
		if err != nil {
			removeHandler()
			return nil, removeHandler, err
		}

		if len(resp.Stacks) == 0 {
			continue
		}

		if resp.Stacks[0].StackStatus == cfTypes.StackStatusCreateComplete {
			outputParams := map[string]string{}
			for _, output := range resp.Stacks[0].Outputs {
				outputParams[*output.OutputKey] = *output.OutputValue
			}

			return outputParams, removeHandler, nil
		} else if resp.Stacks[0].StackStatus == cfTypes.StackStatusCreateFailed ||
			resp.Stacks[0].StackStatus == cfTypes.StackStatusRollbackComplete ||
			resp.Stacks[0].StackStatus == cfTypes.StackStatusRollbackFailed ||
			resp.Stacks[0].StackStatus == cfTypes.StackStatusDeleteFailed ||
			resp.Stacks[0].StackStatus == cfTypes.StackStatusDeleteComplete {
			var reason string
			if resp.Stacks[0].StackStatusReason != nil {
				reason = *resp.Stacks[0].StackStatusReason
			} else {
				reasons, err := p.getFailureReasons(ctx, stackName)
				if err != nil {
					log.Error("Failed to get stack events", "err", err)
				} else {
					reason = strings.Join(reasons, ", ")
				}
			}

			log.Error("Stack creation failed", "reason", reason)
			removeHandler()
			return nil, removeHandler, errors.New("stack creation failed")
		}
	}
}

func (p *AwsProvisioner) deleteStack(ctx context.Context, stackName string) error {
	log.Debug("Deleting", "stackName", stackName)
	_, err := p.cfClient.DeleteStack(ctx, &cloudformation.DeleteStackInput{
		StackName: pstr(stackName),
	})
	if err != nil {
		return err
	}

	// wait for stack to be deleted
	for {
		status, err := p.cfClient.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
			StackName: pstr(stackName),
		})
		if err != nil {
			if strings.Contains(err.Error(), "ValidationError") && strings.Contains(err.Error(), "does not exist") {
				return nil
			}
			return err
		}

		if len(status.Stacks) == 0 {
			return nil
		}

		if status.Stacks[0].StackStatus == cfTypes.StackStatusDeleteComplete {
			return nil
		}

		if status.Stacks[0].StackStatus == cfTypes.StackStatusDeleteFailed {
			return errors.New("stack deletion failed")
		}

		log.Debug("Deleting...", "stackName", stackName)

		time.Sleep(10 * time.Second)
	}
}

func (p *AwsProvisioner) deleteBucket(ctx context.Context, bucketName string) error {
	log.Debug("Empty bucket", "bucketName", bucketName)
	listResp, err := p.s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: pstr(bucketName),
	})
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchBucket") {
			return nil
		}

		return err
	}

	for _, obj := range listResp.Contents {
		_, err := p.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: pstr(bucketName),
			Key:    obj.Key,
		})

		if err != nil {
			log.Error("Failed to delete object", "err", err)
			continue
		}
	}

	log.Debug("Emptying bucket versions", "bucketName", bucketName)
	listVersResp, err := p.s3Client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: pstr(bucketName),
	})
	if err != nil {
		return err
	}
	var deleteObjects []s3.DeleteObjectInput

	for _, obj := range listVersResp.Versions {
		deleteObjects = append(deleteObjects, s3.DeleteObjectInput{
			Bucket:    pstr(bucketName),
			Key:       obj.Key,
			VersionId: obj.VersionId,
		})
	}

	for _, obj := range listVersResp.DeleteMarkers {
		deleteObjects = append(deleteObjects, s3.DeleteObjectInput{
			Bucket:    pstr(bucketName),
			Key:       obj.Key,
			VersionId: obj.VersionId,
		})
	}

	for _, obj := range deleteObjects {
		log.Debug("Deleting object version", "key", *obj.Key, "versionId", *obj.VersionId)
		_, err := p.s3Client.DeleteObject(ctx, &obj)

		if err != nil {
			log.Error("Failed to delete object version", "err", err)
			continue
		}
	}

	log.Debug("Deleting bucket", "bucketName", bucketName)
	_, err = p.s3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: pstr(bucketName),
	})
	if err != nil {
		return err
	}

	return nil
}

func (p *AwsProvisioner) getFailureReasons(ctx context.Context, stackName string) ([]string, error) {
	events, err := p.cfClient.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{
		StackName: pstr(stackName),
	})
	if err != nil {
		return nil, err
	}
	var reasons []string

	for _, event := range events.StackEvents {
		if event.ResourceStatus == cfTypes.ResourceStatusCreateFailed {
			reasons = append(reasons, *event.ResourceStatusReason)
		}
	}

	return reasons, nil
}

func (p *AwsProvisioner) waitUntilUp(ctx context.Context, instanceId string) error {
	log.Debug("Waiting for instance to be up", "instanceId", instanceId)

	const timeout = 5 * time.Minute

	timeoutTime := time.Now().Add(timeout)
	var lastError error

	for {
		if time.Now().After(timeoutTime) {
			return errors.Join(errors.New("timeout waiting for instance to be up"), lastError)
		}

		stdout, _, err := p.runShell(ctx, instanceId, "printf 1")
		if stdout == "1" {
			return nil
		}
		if err != nil {
			lastError = err
		}

		time.Sleep(10 * time.Second)
	}
}

func (p *AwsProvisioner) runShell(ctx context.Context, instanceId string, script string) (stdout, stderr string, err error) {
	log.Debug("Running shell script", "instanceId", instanceId)
	res, err := p.ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: pstr("AWS-RunShellScript"),
		InstanceIds:  []string{instanceId},
		Parameters: map[string][]string{
			"commands": {
				script,
			},
		},
	})
	if err != nil {
		return "", "", err
	}

	// wait for command to finish
	for {
		time.Sleep(10 * time.Second)
		resp, err := p.ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  res.Command.CommandId,
			InstanceId: pstr(instanceId),
		})
		if err != nil {
			return "", "", err
		}

		log.Debug("Command status", "status", resp.Status)

		if resp.Status == ssmTypes.CommandInvocationStatusSuccess {
			if resp.ResponseCode != 0 {
				return *resp.StandardOutputContent, *resp.StandardErrorContent, errors.New("command failed")
			}

			return *resp.StandardOutputContent, *resp.StandardErrorContent, nil
		} else if resp.Status == ssmTypes.CommandInvocationStatusFailed {
			return *resp.StandardOutputContent, *resp.StandardErrorContent, errors.New("command failed")
		} else if resp.Status == ssmTypes.CommandInvocationStatusTimedOut {
			return *resp.StandardOutputContent, *resp.StandardErrorContent, errors.New("command timed out")
		} else if resp.Status == ssmTypes.CommandInvocationStatusCancelling {
			return *resp.StandardOutputContent, *resp.StandardErrorContent, errors.New("command was cancelled")
		} else if resp.Status == ssmTypes.CommandInvocationStatusCancelled {
			return *resp.StandardOutputContent, *resp.StandardErrorContent, errors.New("command was cancelled")
		}
	}
}

func pstr(s string) *string {
	return &s
}

func (p *AwsProvisioner) Locations(ctx context.Context) ([]provision.Location, error) {
	return locations, nil
}

func (p *AwsProvisioner) initSdkClients(ctx context.Context, region string) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}

	cfg.Region = region

	p.stsClient = sts.NewFromConfig(cfg)
	p.cfClient = cloudformation.NewFromConfig(cfg)
	p.ssmClient = ssm.NewFromConfig(cfg)
	p.s3Client = s3.NewFromConfig(cfg)
	p.ec2Client = ec2.NewFromConfig(cfg)

	return nil
}
