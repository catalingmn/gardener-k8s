// Copyright (c) 2023 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package botanist

import (
	"context"

	"k8s.io/utils/pointer"

	v1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	"github.com/gardener/gardener/pkg/operation/botanist/component"
	"github.com/gardener/gardener/pkg/operation/botanist/component/nginxingressshoot"
	"github.com/gardener/gardener/pkg/utils"
	"github.com/gardener/gardener/pkg/utils/images"
	"github.com/gardener/gardener/pkg/utils/imagevector"
)

// DefaultNginxIngress returns a deployer for the nginxingress.
func (b *Botanist) DefaultNginxIngress() (component.DeployWaiter, error) {
	imageController, err := b.ImageVector.FindImage(images.ImageNameNginxIngressController, imagevector.RuntimeVersion(b.ShootVersion()), imagevector.TargetVersion(b.ShootVersion()))
	if err != nil {
		return nil, err
	}
	imageDefaultBackend, err := b.ImageVector.FindImage(images.ImageNameIngressDefaultBackend, imagevector.RuntimeVersion(b.ShootVersion()), imagevector.TargetVersion(b.ShootVersion()))
	if err != nil {
		return nil, err
	}

	values := nginxingressshoot.Values{
		NginxControllerImage: imageController.String(),
		DefaultBackendImage:  imageDefaultBackend.String(),
		KubernetesVersion:    b.Shoot.KubernetesVersion,
		VPAEnabled:           b.Shoot.WantsVerticalPodAutoscaler,
		PSPDisabled:          b.Shoot.PSPDisabled,
	}

	if b.APIServerSNIEnabled() {
		values.KubeAPIServerHost = pointer.String(b.outOfClusterAPIServerFQDN())
	}

	if nginxIngressSpec := b.Shoot.GetInfo().Spec.Addons.NginxIngress; nginxIngressSpec != nil {
		values.ConfigData = getConfig(nginxIngressSpec.Config)

		if nginxIngressSpec.LoadBalancerSourceRanges != nil {
			values.LoadBalancerSourceRanges = nginxIngressSpec.LoadBalancerSourceRanges
		}
		if nginxIngressSpec.ExternalTrafficPolicy != nil {
			values.ExternalTrafficPolicy = *nginxIngressSpec.ExternalTrafficPolicy
		}
	}

	return nginxingressshoot.New(b.SeedClientSet.Client(), b.Shoot.SeedNamespace, values), nil
}

// DeployNginxIngressAddon deploys the NginxIngress Addon component.
func (b *Botanist) DeployNginxIngressAddon(ctx context.Context) error {
	if !v1beta1helper.NginxIngressEnabled(b.Shoot.GetInfo().Spec.Addons) {
		return b.Shoot.Components.Addons.NginxIngress.Destroy(ctx)
	}

	return b.Shoot.Components.Addons.NginxIngress.Deploy(ctx)
}

func getConfig(config map[string]string) map[string]string {
	var (
		defaultConfig = map[string]string{
			"server-name-hash-bucket-size": "256",
			"use-proxy-protocol":           "false",
			"worker-processes":             "2",
		}
	)
	if config != nil {
		return utils.MergeStringMaps(defaultConfig, config)
	}
	return defaultConfig
}