{ pkgs, ... }:
{
  processes.web.exec = "npm run dev";
  processes = {
    worker.exec = "php artisan queue:work";
  };
  services.mysql.enable = true;
  services = {
    redis.enable = true;
    caddy = { enable = true; };
  };
}
