`docker-default.json` is Docker's default seccomp profile, copied unchanged
from moby v27.5.1 (`profiles/seccomp/default.json`, Apache License 2.0,
https://github.com/moby/moby). The agent takes `symlink` and `symlinkat` out
of it for the containers that run a PHP site's code (#50): the edge sees every
site's files, so a link made by one site's code could serve another site's.
