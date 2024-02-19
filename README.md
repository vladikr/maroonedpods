# MaroonedPods [WIP]
 
An operator that enables container workloads to be isolated in Virtual Machines using KubeVirt.


## Here's a brief overview of how it works:

- Any submitted pod tagged with a "maroonedPods" label will be admitted but will be prevented from scheduling.

- At this point, a new Virtual Machine will be created. It will run a Kubernetes Node image. 

- On boot, this VM  will register itself as a Node within the cluster, specifically allocated for the awaiting pod.

- As soon as this Node becomes ready, the Scheduling Gate on the pod is removed, allowing it to be scheduled to this newly prepared Node.


<br/>

MaroonedPods is taking the Kubernetes native approach to the workload isolation problem.
<br/>
The project lets you run Pod in virtual machines (VMs). These pods can use all the available Kubernetes plugins and access special hardware just like any other app running on KubeVirt VMs.


<br/>



[Demo](https://private-user-images.githubusercontent.com/1035064/303094621-139f906f-f01c-4497-9a1e-4bb4215617ad.webm?jwt=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJnaXRodWIuY29tIiwiYXVkIjoicmF3LmdpdGh1YnVzZXJjb250ZW50LmNvbSIsImtleSI6ImtleTUiLCJleHAiOjE3MDczMzAyMjQsIm5iZiI6MTcwNzMyOTkyNCwicGF0aCI6Ii8xMDM1MDY0LzMwMzA5NDYyMS0xMzlmOTA2Zi1mMDFjLTQ0OTctOWExZS00YmI0MjE1NjE3YWQud2VibT9YLUFtei1BbGdvcml0aG09QVdTNC1ITUFDLVNIQTI1NiZYLUFtei1DcmVkZW50aWFsPUFLSUFWQ09EWUxTQTUzUFFLNFpBJTJGMjAyNDAyMDclMkZ1cy1lYXN0LTElMkZzMyUyRmF3czRfcmVxdWVzdCZYLUFtei1EYXRlPTIwMjQwMjA3VDE4MTg0NFomWC1BbXotRXhwaXJlcz0zMDAmWC1BbXotU2lnbmF0dXJlPTg1NjkwM2Y2NWQ1ZDYxMmEyNzU2MzczYTMxZmM2MzM4YTJhNWViMDY2NmUwNzlhOWVmZWIwZWYwMDIxZmFmZTEmWC1BbXotU2lnbmVkSGVhZGVycz1ob3N0JmFjdG9yX2lkPTAma2V5X2lkPTAmcmVwb19pZD0wIn0.XxqNcdyEmLAUjwhA9g9O0tTHCMBHW-qm2zsO_h6eWWk)
