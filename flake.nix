{
  description = "FleetCom — fleet management & agent monitoring platform";

  outputs =
    { self, ... }:
    {
      nixosModules.fleetcom-agent = import ./nix/module.nix;
      nixosModules.default = self.nixosModules.fleetcom-agent;
    };
}
