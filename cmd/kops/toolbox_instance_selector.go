/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/aws/amazon-ec2-instance-selector/pkg/cli"
	"github.com/aws/amazon-ec2-instance-selector/pkg/selector"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kops/cmd/kops/util"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/client/simple"
	"k8s.io/kops/upup/pkg/fi/cloudup"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

// Filter Flag Constants
const (
	vcpus                  = "vcpus"
	memory                 = "memory"
	vcpusToMemoryRatio     = "vcpus-to-memory-ratio"
	cpuArchitecture        = "cpu-architecture"
	gpus                   = "gpus"
	gpuMemoryTotal         = "gpu-memory-total"
	placementGroupStrategy = "placement-group-strategy"
	usageClass             = "usage-class"
	enaSupport             = "ena-support"
	burstSupport           = "burst-support"
	availabilityZones      = "zones"
	networkInterfaces      = "network-interfaces"
	networkPerformance     = "network-performance"
	allowList              = "allow-list"
	denyList               = "deny-list"
	maxResults             = "max-results"
)

// Aggregate Filter Flag Constants
const (
	instanceTypeBase   = "instance-type-base"
	flexible           = "flexible"
	instanceGroupCount = "instance-group-count"
)

// Control Flag Constants
const (
	nodeCountMin       = "node-count-min"
	nodeCountMax       = "node-count-max"
	nodeVolumeSize     = "node-volume-size"
	nodeSecurityGroups = "node-security-groups"
	clusterAutoscaler  = "cluster-autoscaler"
	usageClassSpot     = "spot"
	usageClassOndemand = "on-demand"
	igName             = "instance-group-name"
	dryRun             = "dry-run"
	output             = "output"
)

const (
	nameRegex = `^[a-zA-Z0-9\-_]{1,128}$`
)

var (
	toolboxInstanceSelectorLong = templates.LongDesc(i18n.T(`
	Generate on-demand or spot instance-group specs by providing resource specs like vcpus and memory rather than instance types.`))

	toolboxInstanceSelectorExample = templates.Examples(i18n.T(`

	## Create a best-practices spot instance-group using a MixInstancesPolicy and Capacity-Optimized spot allocation strategy
	## --flexible defaults to a 1:2 vcpus to memory ratio and 4 vcpus
	kops toolbox instance-selector --usage-class spot --instance-group-name my-spot-mig --flexible

	## Create a best-practices on-demand instance-group with custom vcpus and memory range filters
	kops toolbox instance-selector --instance-group-name my-ondemand-ig \
	  --vcpus-min=2 --vcpus-max=4  --memory-min 2048 --memory-max 4096
	`))

	toolboxInstanceSelectorShort = i18n.T(`Generate on-demand or spot instance-group specs by providing resource specs like vcpus and memory.`)
)

// NewCmdToolboxInstanceSelector defines the cobra command for the instance-selector tool
func NewCmdToolboxInstanceSelector(f *util.Factory, out io.Writer) *cobra.Command {
	commandline := cli.New(
		"instance-selector",
		toolboxInstanceSelectorShort,
		toolboxInstanceSelectorLong,
		toolboxInstanceSelectorExample,
		func(cmd *cobra.Command, args []string) {},
	)
	commandline.Command.Run = func(cmd *cobra.Command, args []string) {
		ctx := context.TODO()

		if err := rootCommand.ProcessArgs(args); err != nil {
			exitWithError(err)
		}
		err := RunToolboxInstanceSelector(ctx, f, out, rootCommand.ClusterName(), &commandline)
		if err != nil {
			exitWithError(err)
		}
	}

	cpuArchs := []string{"x86_64", "i386", "arm64"}
	cpuArchDefault := "x86_64"
	placementGroupStrategies := []string{"cluster", "partition", "spread"}
	usageClasses := []string{usageClassSpot, usageClassOndemand}
	usageClassDefault := usageClassOndemand
	nodeCountMinDefault := 2
	nodeCountMaxDefault := 15
	maxResultsDefault := 20

	commandline.BoolFlag(clusterAutoscaler, nil, nil, "Add auto-discovery tags for cluster-autoscaler to manage the instance-group")
	commandline.StringFlag(igName, nil, nil, "Name of the Instance-Group", func(val interface{}) error {
		if val == nil {
			return fmt.Errorf("error you must supply --%s", igName)
		}
		matched, err := regexp.MatchString(nameRegex, *val.(*string))
		if err != nil {
			return err
		}
		if matched {
			return nil
		}
		return fmt.Errorf("error --%s must conform to the regex: \"%s\"", igName, nameRegex)
	})

	// Instance Group Node Configurations

	commandline.IntFlag(nodeCountMin, nil, &nodeCountMinDefault, "Set the minimum number of nodes")
	commandline.IntFlag(nodeCountMax, nil, &nodeCountMaxDefault, "Set the maximum number of nodes")
	commandline.IntFlag(nodeVolumeSize, nil, nil, "Set instance volume size (in GiB) for nodes")
	commandline.StringSliceFlag(nodeSecurityGroups, nil, nil, "Add precreated additional security groups to nodes")

	// Aggregate Filters

	commandline.StringFlag(instanceTypeBase, nil, nil, "Base instance type to retrieve similarly spec'd instance types", nil)
	commandline.BoolFlag(flexible, nil, nil, "Retrieves a group of instance types spanning multiple generations based on opinionated defaults and user overridden resource filters")
	commandline.IntFlag(instanceGroupCount, nil, nil, "Number of instance groups to create w/ different vcpus-to-memory-ratios starting at 1:2 and doubling.")

	// Raw Filters

	commandline.IntMinMaxRangeFlags(vcpus, nil, nil, "Number of vcpus available to the instance type.")
	commandline.IntMinMaxRangeFlags(memory, nil, nil, "Amount of memory available in MiB (Example: 4096)")
	commandline.RatioFlag(vcpusToMemoryRatio, nil, nil, "The ratio of vcpus to memory in MiB. (Example: 1:2)")
	commandline.StringOptionsFlag(cpuArchitecture, nil, &cpuArchDefault, fmt.Sprintf("CPU architecture [%s]", strings.Join(cpuArchs, ", ")), cpuArchs)
	commandline.IntMinMaxRangeFlags(gpus, nil, nil, "Total number of GPUs (Example: 4)")
	commandline.IntMinMaxRangeFlags(gpuMemoryTotal, nil, nil, "Number of GPUs' total memory in MiB (Example: 4096)")
	commandline.StringOptionsFlag(placementGroupStrategy, nil, nil, fmt.Sprintf("Placement group strategy: [%s]", strings.Join(placementGroupStrategies, ", ")), placementGroupStrategies)
	commandline.StringOptionsFlag(usageClass, nil, &usageClassDefault, fmt.Sprintf("Usage class: [%s]", strings.Join(usageClasses, ", ")), usageClasses)
	commandline.BoolFlag(enaSupport, nil, nil, "Instance types where ENA is supported or required")
	commandline.BoolFlag(burstSupport, nil, nil, "Burstable instance types")
	commandline.StringSliceFlag(availabilityZones, nil, nil, "Availability zones or zone ids to check only EC2 capacity offered in those specific AZs")
	commandline.IntMinMaxRangeFlags(networkInterfaces, nil, nil, "Number of network interfaces (ENIs) that can be attached to the instance")
	commandline.RegexFlag(allowList, nil, nil, "List of allowed instance types to select from w/ regex syntax (Example: m[3-5]\\.*)")
	commandline.RegexFlag(denyList, nil, nil, "List of instance types which should be excluded w/ regex syntax (Example: m[1-2]\\.*)")

	commandline.IntFlag(maxResults, nil, &maxResultsDefault, "Maximum number of instance types to return back")
	commandline.BoolFlag(dryRun, nil, nil, "If true, only print the object that would be sent, without sending it. This flag can be used to create a cluster YAML or JSON manifest.")
	commandline.StringFlag(output, nil, commandline.StringMe("o"), "Output format. One of json|yaml. Used with the --dry-run flag.", nil)

	return commandline.Command
}

func processAndValidateFlags(commandline *cli.CommandLineInterface, clusterName string) error {
	if err := commandline.SetUntouchedFlagValuesToNil(); err != nil {
		return err
	}

	if err := commandline.ProcessRangeFilterFlags(); err != nil {
		return err
	}

	if err := commandline.ValidateFlags(); err != nil {
		return err
	}

	if clusterName == "" {
		return fmt.Errorf("ClusterName is required")
	}

	return nil
}

func retrieveClusterRefs(ctx context.Context, f *util.Factory, clusterName string) (simple.Clientset, *kops.Cluster, *kops.Channel, error) {
	clientset, err := f.Clientset()
	if err != nil {
		return nil, nil, nil, err
	}

	cluster, err := clientset.GetCluster(ctx, clusterName)
	if err != nil {
		return nil, nil, nil, err
	}

	if cluster == nil {
		return nil, nil, nil, fmt.Errorf("cluster %q not found", clusterName)
	}

	channel, err := cloudup.ChannelForCluster(cluster)
	if err != nil {
		return nil, nil, nil, err
	}

	if len(cluster.Spec.Subnets) == 0 {
		return nil, nil, nil, fmt.Errorf("configuration must include Subnets")
	}

	return clientset, cluster, channel, nil
}

// RunToolboxInstanceSelector executes the instance-selector tool to create instance groups with declarative resource specifications
func RunToolboxInstanceSelector(ctx context.Context, f *util.Factory, out io.Writer, clusterName string, commandline *cli.CommandLineInterface) error {

	if err := processAndValidateFlags(commandline, clusterName); err != nil {
		return err
	}

	clientset, cluster, channel, err := retrieveClusterRefs(ctx, f, clusterName)
	if err != nil {
		return err
	}

	zones, region, err := getClusterZonesAndRegion(cluster)
	if err != nil {
		return err
	}

	tags := map[string]string{"KubernetesCluster": clusterName}
	cloud, err := awsup.NewAWSCloud(region, tags)
	if err != nil {
		return fmt.Errorf("error initializing AWS client: %v", err)
	}

	flags := commandline.Flags

	instanceSelector := selector.Selector{
		EC2: cloud.EC2(),
	}

	igCount := commandline.IntMe(flags[instanceGroupCount])
	if igCount == nil || *igCount == 0 {
		count := 1
		igCount = &count
	}
	instanceGroupName := *commandline.StringMe(flags[igName])
	filters := getFilters(commandline, flags, region)

	var minSize *int32 = nil
	var maxSize *int32 = nil
	var volumeSize *int32 = nil
	var securityGroups []string = nil

	if flags[nodeCountMin] != nil {
		ncmin := int32(*commandline.IntMe(flags[nodeCountMin]))
		minSize = &ncmin
	}
	if flags[nodeCountMax] != nil {
		ncmax := int32(*commandline.IntMe(flags[nodeCountMax]))
		maxSize = &ncmax
	}
	if flags[nodeVolumeSize] != nil {
		nvsize := int32(*commandline.IntMe(flags[nodeVolumeSize]))
		volumeSize = &nvsize
	}
	if flags[nodeSecurityGroups] != nil {
		nSecurityGroups := *commandline.StringSliceMe(flags[nodeSecurityGroups])
		if len(nSecurityGroups) > 0 {
			securityGroups = nSecurityGroups
		}
	}

	mutatedFilters := filters
	if flags[instanceGroupCount] != nil || flags[flexible] != nil {
		if filters.VCpusToMemoryRatio == nil {
			defaultStartRatio := float64(2.0)
			mutatedFilters.VCpusToMemoryRatio = &defaultStartRatio
		}
	}

	newInstanceGroups := []*kops.InstanceGroup{}

	for i := 0; i < *igCount; i++ {
		igNameForRun := instanceGroupName
		if *igCount != 1 {
			igNameForRun = fmt.Sprintf("%s%d", instanceGroupName, i+1)
		}
		selectedInstanceTypes, err := instanceSelector.Filter(mutatedFilters)
		if err != nil {
			return fmt.Errorf("error finding matching instance types")
		}
		if len(selectedInstanceTypes) == 0 {
			return fmt.Errorf("no instance types were returned becasue the criteria specified was too narrow")
		}
		usageClass := *commandline.StringMe(flags[usageClass])

		ig := createInstanceGroup(igNameForRun, clusterName, zones)
		ig = decorateWithInstanceGroupSpecs(ig, minSize, maxSize, volumeSize, securityGroups)
		ig, err = decorateWithMixedInstancesPolicy(ig, usageClass, selectedInstanceTypes)
		if err != nil {
			return err
		}
		if flags[clusterAutoscaler] != nil && *commandline.BoolMe(flags[clusterAutoscaler]) {
			ig = decorateWithClusterAutoscalerLabels(ig)
		}
		ig, err = cloudup.PopulateInstanceGroupSpec(cluster, ig, channel)
		if err != nil {
			return err
		}

		newInstanceGroups = append(newInstanceGroups, ig)

		if *igCount != 1 {
			doubledRatio := (*mutatedFilters.VCpusToMemoryRatio) * 2
			mutatedFilters.VCpusToMemoryRatio = &doubledRatio
		}
	}

	if flags[dryRun] != nil && *commandline.BoolMe(flags[dryRun]) {
		outputOption := commandline.StringMe(flags[output])
		if flags[output] == nil || *outputOption == "" {
			return fmt.Errorf("must set output flag; yaml or json")
		}

		for _, ig := range newInstanceGroups {
			switch *outputOption {
			case OutputYaml:
				if err := fullOutputYAML(out, ig); err != nil {
					return fmt.Errorf("error writing cluster yaml to stdout: %v", err)
				}
			case OutputJSON:
				if err := fullOutputJSON(out, ig); err != nil {
					return fmt.Errorf("error writing cluster json to stdout: %v", err)
				}
			default:
				return fmt.Errorf("unsupported output type %q", *outputOption)
			}
		}
		return nil
	}

	for _, ig := range newInstanceGroups {
		_, err = clientset.InstanceGroupsFor(cluster).Create(ctx, ig, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("error storing InstanceGroup: %v", err)
		}

		if err := fullOutputYAML(out, ig); err != nil {
			return fmt.Errorf("error writing cluster yaml to stdout: %v", err)
		}
	}

	return nil
}

func getFilters(commandline *cli.CommandLineInterface, flags map[string]interface{}, region string) selector.Filters {
	return selector.Filters{
		VCpusRange:             commandline.IntRangeMe(flags[vcpus]),
		MemoryRange:            commandline.IntRangeMe(flags[memory]),
		VCpusToMemoryRatio:     commandline.Float64Me(flags[vcpusToMemoryRatio]),
		CPUArchitecture:        commandline.StringMe(flags[cpuArchitecture]),
		GpusRange:              commandline.IntRangeMe(flags[gpus]),
		GpuMemoryRange:         commandline.IntRangeMe(flags[gpuMemoryTotal]),
		PlacementGroupStrategy: commandline.StringMe(flags[placementGroupStrategy]),
		UsageClass:             commandline.StringMe(flags[usageClass]),
		EnaSupport:             commandline.BoolMe(flags[enaSupport]),
		Burstable:              commandline.BoolMe(flags[burstSupport]),
		Region:                 commandline.StringMe(region),
		AvailabilityZones:      commandline.StringSliceMe(flags[availabilityZones]),
		MaxResults:             commandline.IntMe(flags[maxResults]),
		NetworkInterfaces:      commandline.IntRangeMe(flags[networkInterfaces]),
		NetworkPerformance:     commandline.IntRangeMe(flags[networkPerformance]),
		AllowList:              commandline.RegexMe(flags[allowList]),
		DenyList:               commandline.RegexMe(flags[denyList]),
		InstanceTypeBase:       commandline.StringMe(flags[instanceTypeBase]),
		Flexible:               commandline.BoolMe(flags[flexible]),
	}
}

func getClusterZonesAndRegion(cluster *kops.Cluster) ([]string, string, error) {
	zones := []string{}
	region := ""
	for _, subnet := range cluster.Spec.Subnets {
		if len(subnet.Name) <= 2 {
			return nil, "", fmt.Errorf("invalid AWS zone: %q", subnet.Zone)
		}
		zones = append(zones, subnet.Zone)
		zoneRegion := subnet.Zone[:len(subnet.Zone)-1]
		if region != "" && zoneRegion != region {
			return nil, "", fmt.Errorf("clusters cannot span multiple regions")
		}

		region = zoneRegion
	}
	return zones, region, nil
}

func createInstanceGroup(groupName, clusterName string, zones []string) *kops.InstanceGroup {
	ig := &kops.InstanceGroup{}
	ig.ObjectMeta.Name = groupName
	ig.Spec.Role = kops.InstanceGroupRoleNode
	ig.Spec.Subnets = zones
	ig.ObjectMeta.Labels = make(map[string]string)
	ig.ObjectMeta.Labels[kops.LabelClusterName] = clusterName

	ig.AddInstanceGroupNodeLabel()
	return ig
}

func decorateWithInstanceGroupSpecs(instanceGroup *kops.InstanceGroup, minNodes, maxNodes, volumeSize *int32, securityGroups []string) *kops.InstanceGroup {
	ig := instanceGroup
	ig.Spec.MinSize = minNodes
	ig.Spec.MaxSize = maxNodes
	ig.Spec.RootVolumeSize = volumeSize
	ig.Spec.AdditionalSecurityGroups = securityGroups
	return ig
}

func decorateWithMixedInstancesPolicy(instanceGroup *kops.InstanceGroup, usageClass string, instanceSelections []string) (*kops.InstanceGroup, error) {
	ig := instanceGroup
	ig.Spec.MachineType = instanceSelections[0]

	if usageClass == usageClassSpot {
		ondemandBase := int64(0)
		ondemandAboveBase := int64(0)
		spotAllocationStrategy := "capacity-optimized"
		ig.Spec.MixedInstancesPolicy = &kops.MixedInstancesPolicySpec{
			Instances:              instanceSelections,
			OnDemandBase:           &ondemandBase,
			OnDemandAboveBase:      &ondemandAboveBase,
			SpotAllocationStrategy: &spotAllocationStrategy,
		}
	} else if usageClass == usageClassOndemand {
		ig.Spec.MixedInstancesPolicy = &kops.MixedInstancesPolicySpec{
			Instances: instanceSelections,
		}
	} else {
		return nil, fmt.Errorf("error node usage class not supported")
	}

	usageClassLabelKey := "kops.k8s.io/instance-selector/usage-class"
	if ig.Spec.CloudLabels == nil {
		ig.Spec.CloudLabels = map[string]string{usageClassLabelKey: usageClass}
	} else {
		ig.Spec.CloudLabels[usageClassLabelKey] = usageClass
	}
	if ig.Spec.NodeLabels == nil {
		ig.Spec.NodeLabels = map[string]string{usageClassLabelKey: usageClass}
	} else {
		ig.Spec.NodeLabels[usageClassLabelKey] = usageClass
	}

	return ig, nil
}

func decorateWithClusterAutoscalerLabels(instanceGroup *kops.InstanceGroup) *kops.InstanceGroup {
	ig := instanceGroup
	clusterName := ig.ObjectMeta.Name
	ig.Spec.CloudLabels["k8s.io/cluster-autoscaler/enabled"] = igName
	ig.Spec.CloudLabels["k8s.io/cluster-autoscaler/"+clusterName] = igName
	return ig
}
