---
title: tow
---

{{< blocks/cover height="auto" >}}
<h1 class="mt-0 mt-md-5"><img src="images/tow-horizontal-dark.svg" alt="tow" width="340" style="max-width: 100%; height: auto"></h1>
<p class="lead mt-4">One file describes your Kubernetes cluster.<br>tow builds it, keeps it as described, and takes it down.</p>
<a class="btn btn-lg btn-primary me-3 mb-4" href="docs/install/">Install</a>
<a class="btn btn-lg btn-secondary me-3 mb-4" href="docs/">Read the docs</a>
<a class="btn btn-lg btn-secondary mb-4" href="https://github.com/towproj/tow"><i class="fab fa-github ms-2"></i> GitHub</a>
{{< /blocks/cover >}}

{{% blocks/lead color="primary" %}}
Describe your cluster in one file: where the machines run, which Kubernetes, and which add-ons. tow builds exactly that. Change the file, run `apply` again, and the running cluster follows.
{{% /blocks/lead %}}

{{% blocks/section type="row" %}}
{{% blocks/feature icon="fa-solid fa-file-lines" title="One file" %}}
Machines, Kubernetes and add-ons in one `tow.yaml`, with ready-made flavors and an [example for every platform](https://github.com/towproj/tow/tree/main/examples).
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-solid fa-arrows-rotate" title="A whole life" %}}
Machines added and removed, settings changed, upgrades one node at a time, etcd backups and restore, certificates renewed.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-solid fa-circle-check" title="Conformant" %}}
Clusters built by tow pass Kubernetes' conformance tests (469/469) on AWS and GCP, after a full lifecycle including an upgrade.
{{% /blocks/feature %}}
{{% /blocks/section %}}
