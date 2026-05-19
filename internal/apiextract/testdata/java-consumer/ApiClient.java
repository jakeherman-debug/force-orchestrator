// Java API client fixture for D15 P5 consumer extractor tests.

package com.example.client;

import org.springframework.web.client.RestTemplate;
import retrofit2.http.*;
import okhttp3.*;

public class ApiClient {

    private RestTemplate restTemplate = new RestTemplate();

    // RestTemplate calls
    public Object getUser(String id) {
        return restTemplate.getForObject("https://api.example.com/users/{id}", Object.class, id);
    }

    public Object createUser(Object request) {
        return restTemplate.postForObject("/api/users", request, Object.class);
    }

    public Object getOrders() {
        return restTemplate.getForEntity("/api/orders", Object.class);
    }

    // OkHttp call
    public Response fetchProfile() throws Exception {
        OkHttpClient client = new OkHttpClient();
        Request request = new Request.Builder()
            .url("https://api.example.com/profile")
            .build();
        return client.newCall(request).execute();
    }

    // Retrofit annotation interface
    interface UserApi {
        @GET("/api/users/{id}")
        Object getUser(@Path("id") String id);

        @POST("/api/users")
        Object createUser(@Body Object body);

        @DELETE("/api/users/{id}")
        Object deleteUser(@Path("id") String id);
    }
}
