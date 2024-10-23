package aws

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/charmbracelet/log"
)

//go:embed cdk.out cdk.out/**/*
var cdkOut embed.FS

type StackAssetJson struct {
	Version string                    `json:"version"`
	Files   map[string]StackAssetFile `json:"files"`
}

type StackAssetFile struct {
	Source struct {
		Path      string `json:"path"`
		Packaging string `json:"packaging"`
	} `json:"source"`
	Destinations map[string]StackAssetFileDestination `json:"destinations"`
}

type StackAssetFileDestination struct {
	BucketName    string `json:"bucketName"`
	ObjectKey     string `json:"objectKey"`
	AssumeRoleArn string `json:"assumeRoleArn"`
}

/*
	{
	  "version": "34.0.0",
	  "artifacts": {
	    "CdkStack.assets": {
	      "type": "cdk:asset-manifest",
	      "properties": {
	        "file": "CdkStack.assets.json",
	        "requiresBootstrapStackVersion": 6,
	        "bootstrapStackVersionSsmParameter": "/cdk-bootstrap/hnb659fds/version"
	      }
	    },
*/
type ManifestJson struct {
	Version   string                      `json:"version"`
	Artifacts map[string]ManifestArtifact `json:"artifacts"`
}

type ManifestArtifact struct {
	Type       string `json:"type"`
	Properties struct {
		File                              string   `json:"file"`
		TemplateFile                      string   `json:"templateFile"`
		TerminationProtection             bool     `json:"terminationProtection"`
		ValidateOnSynth                   bool     `json:"validateOnSynth"`
		AssumeRoleArn                     string   `json:"assumeRoleArn"`
		CloudFormationExecutionRoleArn    string   `json:"cloudFormationExecutionRoleArn"`
		StackTemplateAssetObjectURL       string   `json:"stackTemplateAssetObjectUrl"`
		RequiresBootstrapStackVersion     int      `json:"requiresBootstrapStackVersion"`
		BootstrapStackVersionSsmParameter string   `json:"bootstrapStackVersionSsmParameter"`
		AdditionalDependencies            []string `json:"additionalDependencies"`
		LookupRole                        struct {
			Arn                               string `json:"arn"`
			RequiresBootstrapStackVersion     int    `json:"requiresBootstrapStackVersion"`
			BootstrapStackVersionSsmParameter string `json:"bootstrapStackVersionSsmParameter"`
		} `json:"lookupRole"`
	} `json:"properties"`
}

type CdkSim struct {
	stsClient *sts.Client
}

func (c *CdkSim) Simulate(ctx context.Context, stsClient *sts.Client) error {
	c.stsClient = stsClient
	return c.uploadAssets(ctx)
}

func (c *CdkSim) uploadAssets(ctx context.Context) error {
	manifestJson := c.loadManifestJson()
	var stackAssumeRole string
	for _, artifact := range manifestJson.Artifacts {
		if artifact.Type == "aws:cloudformation:stack" {
			stackAssumeRole = artifact.Properties.AssumeRoleArn
			break
		}
	}

	err := c.assumeRoleStsClient(ctx, stackAssumeRole, func(stsClient *sts.Client) error {
		c.innerUploadAssets(ctx, stsClient)
		return nil
	})

	return err
}

func (c *CdkSim) innerUploadAssets(ctx context.Context, stsClient *sts.Client) {
	assetManifestJson := c.loadAssetManifestJson()
	for _, file := range assetManifestJson.Files {
		assetFile, err := c.packageFilesToUpload(file.Source.Packaging, file.Source.Path)
		if err != nil {
			log.Error("Failed to package files", "err", err)
			continue
		}

		for _, destination := range file.Destinations {
			err = c.assumeRoleS3Client(ctx, stsClient, destination.AssumeRoleArn, func(s3Client *s3.Client) error {
				log.Info("Uploading asset", "bucketName", destination.BucketName, "objectKey", destination.ObjectKey)

				_, err := s3Client.PutObject(ctx, &s3.PutObjectInput{
					Bucket: &destination.BucketName,
					Key:    &destination.ObjectKey,
					Body:   strings.NewReader(string(assetFile)),
				})

				return err
			})

			if err != nil {
				log.Error("Failed to upload asset", "err", err)
			}
		}
	}
}

func (c *CdkSim) packageFilesToUpload(packingType, path string) ([]byte, error) {
	var assetFile []byte
	var err error
	if packingType == "zip" {
		assetFile, err = c.zipDirContent("cdk.out/" + path)
		if err != nil {
			return nil, err
		}
	} else if packingType == "file" {
		assetFile, err = cdkOut.ReadFile("cdk.out/" + path)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("unknown packing type")
	}

	return assetFile, nil
}

// func expandAllAwsVariables(ctx context.Context, obj any, args map[string]string) {
// 	typeOf := reflect.TypeOf(obj)
// 	valueOf := reflect.ValueOf(obj)

// 	for i := 0; i < typeOf.NumField(); i++ {
// 		field := typeOf.Field(i)
// 		value := valueOf.Field(i)

// 		if field.Type.Kind() == reflect.String {
// 			value.SetString(expandAwsVariables(ctx, args["accountId"], args["region"], value.String()))
// 		} else if field.Type.Kind() == reflect.Ptr && field.Type.Elem().Kind() == reflect.String {
// 			value.Set(reflect.ValueOf(pstr(expandAwsVariables(ctx, args["accountId"], args["region"], *value.Interface().(*string))))
// 		} else if field.Type.Kind() == reflect.Struct {
// 			expandAllAwsVariables(ctx, value.Interface(), args)
// 		} else if field.Type.Kind() == reflect.Ptr && field.Type.Elem().Kind() == reflect.Struct {
// 			expandAllAwsVariables(ctx, value.Interface(), args)
// 		}
// 	}
// }

func expandAwsVariables(ctx context.Context, stsClient *sts.Client, s string) string {
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		log.Error("Failed to get caller identity", "err", err)
		return s
	}

	args := map[string]string{
		"AccountId": *identity.Account,
		"Region":    stsClient.Options().Region,
		"Partition": "aws",
	}

	reg := regexp.MustCompile(`\$\{AWS::([a-zA-Z0-9]+)\}`)
	return reg.ReplaceAllStringFunc(s, func(match string) string {
		key := strings.TrimSuffix(strings.TrimPrefix(match, "${AWS::"), "}")

		if val, ok := args[key]; ok {
			return val
		}

		return match

		// case "AccountId":
		// 	return accountId
		// case "Region":
		// 	return region
		// case "Partition":
		// 	return "aws"
	})
}

func (c *CdkSim) zipDirContent(dir string) ([]byte, error) {
	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)
	defer zipWriter.Close()

	err := c.walkDir(cdkOut, dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		fileBytes, err := cdkOut.ReadFile(path)
		if err != nil {
			return err
		}

		zipFilePath := strings.TrimPrefix(path, dir+"/")
		zipFile, err := zipWriter.Create(zipFilePath)
		if err != nil {
			return err
		}

		_, err = zipFile.Write(fileBytes)
		return err
	})

	if err != nil {
		return nil, err
	}

	zipWriter.Close()

	return buf.Bytes(), nil
}

func (c *CdkSim) walkDir(fs embed.FS, dir string, cb func(path string, info os.FileInfo, err error) error) error {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		path := dir + "/" + entry.Name()
		info, err := entry.Info()
		if err != nil {
			return err
		}

		err = cb(path, info, nil)
		if err != nil {
			return err
		}

		if info.IsDir() {
			err = c.walkDir(fs, path, cb)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *CdkSim) assumeRoleS3Client(ctx context.Context, stsClient *sts.Client, roleArn string, cb func(s3Client *s3.Client) error) error {
	var innerErr error
	_, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         pstr(roleArn),
		RoleSessionName: pstr("wg-ondemand-asset-upload"),
	}, func(req *sts.Options) {
		s3Client := s3.NewFromConfig(aws.Config{
			Credentials: req.Credentials,
			Region:      stsClient.Options().Region,
		})

		innerErr = cb(s3Client)
	})

	if err != nil {
		return err
	}

	return innerErr
}

func (c *CdkSim) assumeRoleStsClient(ctx context.Context, roleArn string, cb func(s3Client *sts.Client) error) error {
	log.Info("Assuming role", "roleArn", roleArn)
	var innerErr error
	_, err := c.stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         pstr(roleArn),
		RoleSessionName: pstr("wg-ondemand-deploy"),
	}, func(req *sts.Options) {
		deeperStsClient := sts.NewFromConfig(aws.Config{
			Credentials: req.Credentials,
			Region:      c.stsClient.Options().Region,
		})

		innerErr = cb(deeperStsClient)
	})

	if err != nil {
		return err
	}

	return innerErr
}

func (c *CdkSim) loadAssetManifestJson() (assetManifestJson StackAssetJson) {
	manifestJson := c.loadManifestJson()
	var assetPath string
	for _, artifact := range manifestJson.Artifacts {
		if artifact.Type == "cdk:asset-manifest" {
			assetPath = artifact.Properties.File
			break
		}
	}

	c.loadCdkOutFile("cdk.out/"+assetPath, &assetManifestJson)
	return
}

func (c *CdkSim) loadManifestJson() (manifestJson ManifestJson) {
	c.loadCdkOutFile("cdk.out/manifest.json", &manifestJson)
	return
}

func (c *CdkSim) loadCdkOutFile(path string, out any) {
	fileBytes, err := cdkOut.ReadFile(path)
	if err != nil {
		panic(err)
	}

	fileBytes = []byte(expandAwsVariables(context.Background(), c.stsClient, string(fileBytes)))

	err = json.Unmarshal(fileBytes, &out)
	if err != nil {
		panic(err)
	}

}
