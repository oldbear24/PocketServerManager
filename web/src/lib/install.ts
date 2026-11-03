/** What the install commands on the "Connect a server" screen are made from. */
export type InstallParams = {
  panel: string;
  caPin: string;
  token: string;
  name: string;
  roles: string[];
  image: string;
  origin: string;
};

export function dockerRunCommand(p: InstallParams): string {
  return [
    "docker run -d --name pocketcloud-agent --restart unless-stopped \\",
    "  -v /var/run/docker.sock:/var/run/docker.sock \\",
    "  -v pocketcloud-agent:/var/lib/pocketcloud \\",
    "  -v /:/host:ro,rslave \\",
    `  -e PANEL=${p.panel} -e CA_PIN=${p.caPin} \\`,
    `  -e TOKEN=${p.token} \\`,
    `  -e NAME=${p.name} -e ROLES=${p.roles.join(",")} \\`,
    `  ${p.image}`,
  ].join("\n");
}

export function composeSnippet(p: InstallParams): string {
  return [
    "services:",
    "  pocketcloud-agent:",
    `    image: ${p.image}`,
    "    container_name: pocketcloud-agent",
    "    restart: unless-stopped",
    "    volumes:",
    "      - /var/run/docker.sock:/var/run/docker.sock",
    "      - pocketcloud-agent:/var/lib/pocketcloud",
    "      - /:/host:ro,rslave",
    "    environment:",
    `      PANEL: ${p.panel}`,
    `      CA_PIN: ${p.caPin}`,
    `      TOKEN: ${p.token}`,
    `      NAME: ${p.name}`,
    `      ROLES: ${p.roles.join(",")}`,
    "",
    "volumes:",
    "  pocketcloud-agent:",
    "    name: pocketcloud-agent",
  ].join("\n");
}

export function scriptCommand(p: InstallParams): string {
  return [
    `curl -fsSL ${p.origin}/api/pc/install.sh | \\`,
    `  TOKEN=${p.token} NAME=${p.name} ROLES=${p.roles.join(",")} sh`,
  ].join("\n");
}
