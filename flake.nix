{
  description = "FleetCom — fleet management & agent monitoring platform";

  outputs =
    { self, ... }:
    {
      nixosModules.fleetcom-bosun = import ./nix/module.nix;
      nixosModules.default = self.nixosModules.fleetcom-bosun;
    };
}
