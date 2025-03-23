from django.db import models

class Server(models.Model):
    host = models.CharField(max_length=255) # Host used to connect to the server
    port = models.IntegerField(default=5572)
    
    public_ip = models.CharField(max_length=255, null=True) # Public IP of the server, to tell the users NEEDS TO BE IMPLEMENTED
    
    online = models.BooleanField(default=False)# NEEDS TO BE IMPLEMENTED
    error = models.TextField(default="") # If the server is offline, this will contain the error message
    
    version = models.CharField(max_length=255, default="") # Version of the rclone server
    
    # Node limits
    data_cap = models.BigIntegerField(default=0) # Data cap in bytes NEEDS TO BE IMPLEMENTED
    network_speed = models.BigIntegerField(default=0) # Network speed in bytes per second
    
    # Information about the provider and location NEEDS TO BE IMPLEMENTED
    provider = models.CharField(max_length=255, default="")
    continent = models.CharField(choices=[
        ("AF", "Africa"),
        ("AN", "Antarctica"),
        ("AS", "Asia"),
        ("EU", "Europe"),
        ("NA", "North America"),
        ("OC", "Oceania"),
        ("SA", "South America"),
    ], max_length=2, default="NA")
    country = models.CharField(max_length=255, default="")
    
    
    def __str__(self):
        return f"{self.host}:{self.port}" 
    
