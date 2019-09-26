# interactive-nomad-attach
Interactively review and attach to containers running in Nomad.

This is a PoC for running a series of calls to [Hashicorp Nomad](https://www.nomadproject.io)
to get information about running jobs and allowing for container attachment
through pre-defined flows. It's meant to be run as an [interactive SSH application](https://drewdevault.com/2019/09/02/Interactive-SSH-programs.html)
behaving similarly to a busybox session.

Taken from Nomad docs:

The general hierarchy for a job is:

```
job
  \_ group
        \_ task
```

Each job file has only a single job, however a job may have multiple groups, and each group may have multiple tasks. Groups contain a set of tasks that are co-located on a machine.

`interactive-nomad-attach` presents an option for users to select an allocation and attach directly against it or select
from a list of jobs and task groups if there are multiple. If users drop through the selection route they are randomly
assigned an allocation meeting that criteria.

See the sample config in the project.

To stand up Nomad, install via brew, apt, etc... and run via: `nomad agent -dev`, for ephemeral, local usage. Then, submit a sample job like:

```hcl
job "redis" {
	datacenters = ["dc1"]
	update {
		stagger = "10s"
		max_parallel = 1
	}
	group "redis" {
		restart {
			attempts = 2
			interval = "1m"
			delay = "10s"
			mode = "fail"
		}
		task "redis" {
			driver = "docker"

			config {
				image = "redis:latest"
				port_map {
					db = 6379
				}
			}

			service {
				name = "${TASKGROUP}-redis"
				port = "db"
				check {
					name = "alive"
					type = "tcp"
					interval = "10s"
					timeout = "2s"
				}
			}
			resources {
				cpu = 500 # 500 MHz
				memory = 256 # 256MB
				network {
					mbits = 10
					port "db" {
					}
				}
			}
		}
	}
}
```

Finally, run `main.go` to interact with the flows.
