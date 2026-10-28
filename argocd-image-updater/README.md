# argocd-image-updater/

The examples of Field Guide 03, *Argo CD Image Updater on kind*, from the Kubernetes Notes zine series. The book runs
Argo CD Image Updater v1.4.0 on Field Guide 02's Argo CD v3.5.6 (this repo's folder `argocd/`, tag `argocd-1.0`) on
No. 00's kind lab.

- `demo-image/`: the book's demo image, a static page that says which tag it is. The workflow
  `.github/workflows/argocd-image-updater-demo-image.yaml` builds and publishes one tag a run, to
  `ghcr.io/<owner>/k8s-notes-examples/image-updater-demo`.

The chapters write to Git: Image Updater's git write-back commits to a branch, and its pull request mode opens pull
requests. Run those chapters in your own fork, as the book says.
